package injector

import (
	"net/http"
)

func RunInjecter(addr, cert, key string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/inject", inject)
	log.Infof("run server at: %s", addr)
	log.Fatalf("listen injecter err: %v", http.ListenAndServeTLS(addr, cert, key, mux))
}
