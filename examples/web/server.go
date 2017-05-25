package main

import (
	"log"
	"net/http"

	"gopkg.in/beatgammit/turnpike.v2"
)

func main() {
	turnpike.Debug()
	s := turnpike.NewBasicWebsocketServer("turnpike.chat.realm")
	http.Handle("/", http.FileServer(http.Dir(".")))
	http.Handle("/ws", s)
	log.Info("turnpike server starting on port 8000")
	log.Info("Hint: start clicking on the web page(s) you open to localhost:8000")
	log.Fatal(http.ListenAndServe(":8000", nil))
}
