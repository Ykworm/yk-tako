package main

import (
	"flag"
	"log"
	"net/http"
)

func main() {
	engine := flag.String("engine", "127.0.0.1:8090", "run engine listen address")
	flag.Parse()

	log.Printf("engine listening on %s (skeleton, no routes)", *engine)
	log.Fatal(http.ListenAndServe(*engine, http.NewServeMux()))
}
