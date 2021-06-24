package main

import "k8s-injector/injector"

func main() {
	injector.RunInjecter(":8082", "simples/server.crt", "simples/server.key")
}
