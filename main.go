package main

import (
	"io"
	"os"
	"net"
	"fmt"
	"os/exec"
	"strings"
	"net/http"
	"strconv"
	"github.com/robfig/cron"
	"sync"
	"flag"
	"regexp"
)

var (
	Name           = "supervisor_exporter"
	listenAddress  = flag.String("unix-sock", "/dev/shm/supervisor_exporter.sock", "Address to listen on for unix sock access and telemetry.")
	metricsPath    = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
)

var g_lock sync.RWMutex
var g_ret string
var g_urlMap map[string]string
var doing bool

type procInfo struct {
	Url string
	Name string
	Fatal float64
	Running float64
}

func substr(str string, start, length int) string {
	rs := []rune(str)
	rl := len(rs)
	end := 0

	if start < 0 {
		start = rl - 1 + start
	}
	end = start + length

	if start > end {
		start, end = end, start
	}

	if start < 0 {
		start = 0
	}
	if start > rl {
		start = rl
	}
	if end < 0 {
		end = 0
	}
	if end > rl {
		end = rl
	}

	return string(rs[start:end])
}

func isURI(uri string) (schema, host string, port int, matched bool) {
	const reExp = `^((?P<schema>((ht|f)tp(s?))|tcp)\://)?((([a-zA-Z0-9_\-]+\.)+[a-zA-Z]{2,})|((?:(?:25[0-5]|2[0-4]\d|[01]\d\d|\d?\d)((\.?\d)\.)){4})|(25[0-5]|2[0-4][0-9]|[0-1]{1}[0-9]{2}|[1-9]{1}[0-9]{1}|[1-9])\.(25[0-5]|2[0-4][0-9]|[0-1]{1}[0-9]{2}|[1-9]{1}[0-9]{1}|[1-9]|0)\.(25[0-5]|2[0-4][0-9]|[0-1]{1}[0-9]{2}|[1-9]{1}[0-9]{1}|[1-9]|0)\.(25[0-5]|2[0-4][0-9]|[0-1]{1}[0-9]{2}|[1-9]{1}[0-9]{1}|[0-9]))(:([0-9]+))?(/[a-zA-Z0-9\-\._\?\,\'/\\\+&amp;%\$#\=~]*)?$`
	pattern := regexp.MustCompile(reExp)
	res := pattern.FindStringSubmatch(uri)
	if len(res) == 0 {
		return
	}
	matched = true
	schema = res[2]
	if schema == "" {
		schema = "tcp"
	}
	host = res[6]
	if res[17] == "" {
		if schema == "https" {
			port = 443
		} else {
			port = 80
		}
	} else {
		port, _ = strconv.Atoi(res[17])
	}

	return
}

func supervisorUrlList() string {
	cmdStr := fmt.Sprintf("ss -tlp |grep supervisord | awk '{print $4}'")
	cmd := exec.Command("/bin/sh", "-c", cmdStr)
	cmd.Wait()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func supervisorDetails(url string) string {
	cmdStr := fmt.Sprintf("/usr/local/supervisor/bin/supervisorctl -s %s status | awk '{print $1,$2}'", url)
	cmd := exec.Command("/bin/sh", "-c", cmdStr)
	cmd.Wait()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func doWork() {
	if doing {
		return
	}
	doing = true

	ret := ""

	var m map[string]*procInfo
	m = make(map[string]*procInfo)
	for _, i := range g_urlMap {
		s := supervisorDetails(i)
		if strings.Contains(s, "refused") || strings.Contains(s, "error: <class") {
			ret += fmt.Sprintf("supervisor_up{url=\"%s\"} 0\n", i)
			continue
		}
		ret += fmt.Sprintf("supervisor_up{url=\"%s\"} 1\n", i)
		s = strings.TrimRight(s, "\n")
		l := strings.Split(s, " ")
		idx := strings.LastIndex(l[0], "_")
		k2 := substr(l[0], 0, idx)
		if v, ok := m[i + "," + k2]; ok {
			if strings.Contains(l[1], "RUNNING") {
				v.Running += 1
			} else {
				v.Fatal += 1
			}
		} else {
			pi := &procInfo{
				Name:k2,
				Fatal:0.0,
				Running:0.0,
			}
			if strings.Contains(l[1], "RUNNING") {
				pi.Running += 1
			} else {
				pi.Fatal += 1
			}
			m[i + "," + k2] = pi
		}
	}

	nameSpace := "supervisor"
    for k, v := range m {
    	l := strings.Split(k, ",")
    	ret += fmt.Sprintf("%s_proc{url=\"%s\",proc=\"%s\",status=\"fatal\"} %g\n",
    		nameSpace, l[0], v.Name, v.Fatal)
		ret += fmt.Sprintf("%s_proc{url=\"%s\",proc=\"%s\",status=\"running\"} %g\n",
			nameSpace, l[0], v.Name, v.Running)
	}


	g_lock.Lock()
	g_ret = ret
	g_lock.Unlock()
	doing = false
}

func metrics(w http.ResponseWriter, req *http.Request) {
	g_lock.RLock()
	io.WriteString(w, g_ret)
	g_lock.RUnlock()
}

func main() {
	flag.Parse()
	addr := ""
	if listenAddress != nil {
		addr = *listenAddress
	} else {
		addr = "/dev/shm/supervisor_exporter.sock"
	}

	g_urlMap = make(map[string]string)
	s := supervisorUrlList()
	s = strings.TrimRight(s, "\n")
	l := strings.Split(s, "\n")
	if len(l) == 0 {
		panic("no supervisors")
	}

	for _, i := range l {
		_, _, _, ok := isURI("http://" + i)
		if ok {
			g_urlMap["http://" + i] = "http://" + i
		}
	}
	if len(g_urlMap) == 0 {
		panic("no supervisors")
	}


	doing = false
	doWork()
	c := cron.New()
	c.AddFunc("0 */2 * * * ?", doWork)
	c.Start()

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", metrics)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>Supervisor Exporter</title></head>
             <body>
             <h1>Supervisor Exporter</h1>
             <p><a href='` + "/metrics" + `'>Metrics</a></p>
             </body>
             </html>`))
	})
	server := http.Server{
		Handler: mux, // http.DefaultServeMux,
	}
	os.Remove(addr)

	listener, err := net.Listen("unix", addr)
	if err != nil {
		panic(err)
	}
	server.Serve(listener)
}