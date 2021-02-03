package main

import (
	"fmt"
	"go.bcc.media/stt"
	"net/http"
)

var fs = map[string]http.HandlerFunc{}

func index(w http.ResponseWriter, r *http.Request) {
	out := ""
	for name := range fs {
		out += fmt.Sprintf("<a href=\"/%s\">%s</a><br />", name, name)
	}
	w.Write([]byte(out))
}

func main() {

	// Add your function here
	fs["Ingest"] = stt.Ingest
	fs["Restult"] = stt.ProcessResults

	for name, handler := range fs {
		http.HandleFunc(fmt.Sprintf("/%s", name), handler)
		fmt.Printf("Handling \"%s\"\n", name)
	}

	http.HandleFunc("/", index)
	fmt.Print("Listening on :8086\n")
	http.ListenAndServe(":8086", nil)
}
