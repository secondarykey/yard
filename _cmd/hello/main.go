// hello is a small web server for trying out yard.
//
//	go build -o hello.exe ./_cmd/hello
//
// Example yard.ini:
//
//	[[server]]
//	name = "hello"
//	path = "/hello"
//	command = "hello.exe"
//	args = ["-port", "{port}", "-base", "{base}"]
package main

import (
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	port := flag.Int("port", 8080, "listen port")
	base := flag.String("base", "/", "base path")
	flag.Parse()

	b := "/" + strings.Trim(*base, "/") + "/"
	if b == "//" {
		b = "/"
	}

	mux := http.NewServeMux()
	mux.HandleFunc(b, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<h1>hello</h1><pre>pid:    %d\nbase:   %s\npath:   %s\nhost:   %s\nxff:    %s\nprefix: %s</pre>",
			os.Getpid(), html.EscapeString(b), html.EscapeString(r.URL.Path), html.EscapeString(r.Host),
			html.EscapeString(r.Header.Get("X-Forwarded-For")), html.EscapeString(r.Header.Get("X-Forwarded-Prefix")))
	})
	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("hello listening on %s%s", addr, b)
	log.Fatal(http.ListenAndServe(addr, mux))
}
