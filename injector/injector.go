package injector

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"gomodules.xyz/jsonpatch/v3"
	kubeApiAdmissionv1 "k8s.io/api/admission/v1"
	kubeApiAdmissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"github.com/sirupsen/logrus"

	"istio.io/istio/pkg/kube"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()

	mu  sync.RWMutex
	log = logrus.New()

	errServiceCodeEmpty = errors.New("service-code not found")

	proxyImage = "nt-sidecar:latest"
	initImage  = "init-proxy:latest"
	logPvcName = "envoylog"
)

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = kubeApiAdmissionv1.AddToScheme(runtimeScheme)
	_ = kubeApiAdmissionv1beta1.AddToScheme(runtimeScheme)
}

const (
	ProxyContainerName        = "nt-proxy"
	InitContainerName         = "nt-init"
	servicePortKey            = "service-port"
	serviceCodeKey            = "service-code"
	groupCodeKey              = "group-code"
	PersistentVolumeClaimName = "envoylog"
)

type InjectionParameters struct {
	pod        *corev1.Pod
	deployMeta *metav1.ObjectMeta
	typeMeta   *metav1.TypeMeta
}

func inject(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := io.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		log.Printf("no body found")
		http.Error(w, "no body found", http.StatusBadRequest)
		return
	}
	// log.Printf("-------------------------%s--------------------------", body)

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Printf("contentType=%s, expect application/json\n", contentType)
		http.Error(w, "invalid Content-Type, want `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	var reviewResponse *kube.AdmissionResponse
	var obj runtime.Object
	var ar *kube.AdmissionReview
	if out, _, err := deserializer.Decode(body, nil, obj); err != nil {
		log.Errorf("Could not decode body: %v", err)
		reviewResponse = toAdmissionResponse(err)
	} else {
		ar, err = kube.AdmissionReviewKubeToAdapter(out)
		if err != nil {
			log.Errorf("Could not decode object: %v", err)
		}
		reviewResponse = doInject(ar)
	}

	response := kube.AdmissionReview{}
	response.Response = reviewResponse
	var responseKube runtime.Object
	var apiVersion string
	if ar != nil {
		apiVersion = ar.APIVersion
		response.TypeMeta = ar.TypeMeta
		if response.Response != nil {
			if ar.Request != nil {
				response.Response.UID = ar.Request.UID
			}
		}
	}
	responseKube = kube.AdmissionReviewAdapterToKube(&response, apiVersion)
	resp, err := json.Marshal(responseKube)
	if err != nil {
		log.Errorf("Could not encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	if _, err := w.Write(resp); err != nil {
		log.Errorf("Could not write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}

func doInject(ar *kube.AdmissionReview) *kube.AdmissionResponse {
	req := ar.Request
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		log.Errorf("Could not unmarshal raw object: %v %s", err,
			string(req.Object.Raw))
		return toAdmissionResponse(err)
	}

	// Deal with potential empty fields, e.g., when the pod is created by a deployment
	podName := potentialPodName(pod.ObjectMeta)
	if pod.ObjectMeta.Namespace == "" {
		pod.ObjectMeta.Namespace = req.Namespace
	}
	log.Infof("Sidecar injection request for %v/%v", req.Namespace, podName)
	log.Debugf("Object: %v", string(req.Object.Raw))
	log.Debugf("OldObject: %v", string(req.OldObject.Raw))

	mu.RLock()

	deploy, typeMeta := kube.GetDeployMetaFromPod(&pod)
	params := InjectionParameters{
		pod:        &pod,
		deployMeta: &deploy,
		typeMeta:   &typeMeta,
	}
	mu.RUnlock()

	patchBytes, err := injectPod(params)
	if err != nil {
		log.Errorf("Pod injection failed: %v", err)
		return toAdmissionResponse(err)
	}

	reviewResponse := kube.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *string {
			pt := "JSONPatch"
			return &pt
		}(),
	}
	return &reviewResponse
}

func injectPod(req InjectionParameters) ([]byte, error) {
	checkPreconditions(req)
	ann := req.pod.Annotations
	serviceCode := ann[serviceCodeKey]
	servicePort := ann[servicePortKey]
	groupCode := ann[groupCodeKey]
	if serviceCode == "" {
		return nil, errServiceCodeEmpty
	}
	if servicePort == "" {
		servicePort = "5000"
	}
	if groupCode == "" {
		groupCode = serviceCode + "_" + serviceCode
	}
	mergedPod := req.pod.DeepCopy()
	if FindSidecar(mergedPod.Spec.Containers) == nil {
		sidecarContainer := NewSidecar(serviceCode, servicePort, groupCode)
		mergedPod.Spec.Containers = append(mergedPod.Spec.Containers, sidecarContainer)
	}

	if FindInitContainer(mergedPod.Spec.InitContainers) == nil {
		initContainer := NewInitContainer(serviceCode, servicePort, groupCode)
		mergedPod.Spec.InitContainers = append(mergedPod.Spec.InitContainers, initContainer)
	}

	if logPvcName != "" {
		mergedPod.Spec.Volumes = append(mergedPod.Spec.Volumes, NewLogPvcVolume(serviceCode, servicePort, groupCode))
	}
	original, err := json.Marshal(req.pod)
	if err != nil {
		return nil, err
	}
	reinjected, err := json.Marshal(mergedPod)
	if err != nil {
		return nil, err
	}
	patch, err := jsonpatch.CreatePatch(original, reinjected)
	if err != nil {
		return nil, err
	}

	return json.Marshal(patch)
}

func NewSidecar(serviceCode, servicePort, groupCode string) corev1.Container {
	c := corev1.Container{
		Name:      ProxyContainerName,
		Image:     proxyImage,
		Ports:     []corev1.ContainerPort{{Name: "http-admin", Protocol: "TCP", ContainerPort: 9901}},
		Env:       []corev1.EnvVar{{Name: "serviceCode", Value: serviceCode}, {Name: "serviceDeployGroupCode", Value: groupCode}, {Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}}},
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{"cpu": resource.MustParse("300m"), "memory": resource.MustParse("512Mi")}, Requests: corev1.ResourceList{"cpu": resource.MustParse("50m"), "memory": resource.MustParse("128Mi")}},
	}
	if logPvcName != "" {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      "envoylog",
			MountPath: "/opt/dataforce/log/envoy",
			SubPath:   groupCode,
		})
	}
	return c
}

func NewInitContainer(serviceCode, servicePort, groupCode string) corev1.Container {
	c := corev1.Container{
		Name:            InitContainerName,
		Image:           initImage,
		Env:             []corev1.EnvVar{{Name: "PROXY_PORT", Value: servicePort}},
		SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}}},
	}
	return c
}

func NewLogPvcVolume(serviceCode, servicePort, groupCode string) corev1.Volume {
	return corev1.Volume{
		Name: "envoylog",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: PersistentVolumeClaimName,
			},
		},
	}
}
