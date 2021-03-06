package handlers

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/turkenh/play-with-ansible/config"
)

func getTargetInfo(vars map[string]string, req *http.Request) (string, string) {
	node := vars["node"]
	port := vars["port"]
	alias := vars["alias"]
	sessionPrefix := vars["session"]
	hostPort := strings.Split(req.Host, ":")

	// give priority to the URL host port
	if len(hostPort) > 1 && hostPort[1] != config.PortNumber {
		port = hostPort[1]
	} else if port == "" {
		port = "80"
	}

	if alias != "" {
		instance := core.InstanceFindByAlias(sessionPrefix, alias)
		if instance != nil {
			node = instance.IP
			return node, port
		}
	}

	// Node is actually an ip, need to convert underscores by dots.
	ip := strings.Replace(node, "-", ".", -1)

	if net.ParseIP(ip) == nil {
		// Not a valid IP, so treat this is a hostname.
	} else {
		node = ip
	}

	return node, port

}

type tcpProxy struct {
	Director func(*http.Request)
	ErrorLog *log.Logger
	Dial     func(network, addr string) (net.Conn, error)
}

func (p *tcpProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logFunc := log.Printf
	if p.ErrorLog != nil {
		logFunc = p.ErrorLog.Printf
	}

	vars := mux.Vars(r)
	instanceIP := vars["node"]

	if i := core.InstanceFindByIP(strings.Replace(instanceIP, "-", ".", -1)); i == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	outreq := new(http.Request)
	// shallow copying
	*outreq = *r
	p.Director(outreq)
	host := outreq.URL.Host

	dial := p.Dial
	if dial == nil {
		dial = net.Dial
	}

	if outreq.URL.Scheme == "wss" || outreq.URL.Scheme == "https" {
		var tlsConfig *tls.Config
		tlsConfig = &tls.Config{InsecureSkipVerify: true}
		dial = func(network, address string) (net.Conn, error) {
			return tls.Dial("tcp", host, tlsConfig)
		}
	}

	d, err := dial("tcp", host)
	if err != nil {
		http.Error(w, "Error forwarding request.", 500)
		logFunc("Error dialing websocket backend %s: %v", outreq.URL, err)
		return
	}
	// All request generated by the http package implement this interface.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Not a hijacker?", 500)
		return
	}
	// Hijack() tells the http package not to do anything else with the connection.
	// After, it bcomes this functions job to manage it. `nc` is of type *net.Conn.
	nc, _, err := hj.Hijack()
	if err != nil {
		logFunc("Hijack error: %v", err)
		return
	}
	defer nc.Close() // must close the underlying net connection after hijacking
	defer d.Close()

	// write the modified incoming request to the dialed connection
	err = outreq.Write(d)
	if err != nil {
		logFunc("Error copying request to target: %v", err)
		return
	}
	errc := make(chan error, 2)
	cp := func(dst io.Writer, src io.Reader) {
		_, err := io.Copy(dst, src)
		errc <- err
	}
	go cp(d, nc)
	go cp(nc, d)
	<-errc
}
func NewTCPProxy() http.Handler {
	director := func(req *http.Request) {
		v := mux.Vars(req)

		node, port := getTargetInfo(v, req)

		if port == "443" {
			if strings.Contains(req.URL.Scheme, "http") {
				req.URL.Scheme = "https"
			} else {
				req.URL.Scheme = "wss"
			}
		}
		req.URL.Host = fmt.Sprintf("%s:%s", node, port)
	}
	return &tcpProxy{Director: director}
}
