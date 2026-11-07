
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	root   = "/sys/fs/cgroup"
	period = int64(100000)
)

type Agent struct {
	mu       sync.Mutex
	id       string
	path     string
	quota    int64
	previous uint64
	lastTime time.Time
	cmd      *exec.Cmd
}

type Metrics struct {
	NodeID      string  `json:"node_id"`
	UsageUsec   uint64  `json:"usage_usec"`
	Utilization float64 `json:"utilization_percent"`
	QuotaCores  float64 `json:"quota_cores"`
	MaxCores    int     `json:"max_cores"`
}

type QuotaRequest struct {
	Cores float64 `json:"cores"`
}

var agent *Agent

func write(path, value string) error {
	return os.WriteFile(path, []byte(value), 0644)
}

func setQuota(cores float64) error {
	if cores < 0.1 {
		cores = 0.1
	}

	max := float64(runtime.NumCPU())

	if cores > max {
		cores = max
	}

	quota := int64(cores * float64(period))

	if err := write(
		filepath.Join(agent.path, "cpu.max"),
		fmt.Sprintf("%d %d", quota, period),
	); err != nil {
		return err
	}

	agent.quota = quota
	return nil
}

func readUsage() (uint64, error) {
	file, err := os.Open(filepath.Join(agent.path, "cpu.stat"))
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())

		if len(fields) == 2 && fields[0] == "usage_usec" {
			return strconv.ParseUint(fields[1], 10, 64)
		}
	}

	return 0, scanner.Err()
}

func startWorkload(w http.ResponseWriter, r *http.Request) {
	agent.mu.Lock()
	defer agent.mu.Unlock()

	if agent.cmd != nil && agent.cmd.Process != nil {
		http.Error(w, "Workload already started", http.StatusConflict)
		return
	}

	cmd := exec.Command(
		"/bin/sh",
		"-c",
		"while :; do :; done",
	)

	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	if err := write(
		filepath.Join(agent.path, "cgroup.procs"),
		strconv.Itoa(cmd.Process.Pid),
	); err != nil {
		cmd.Process.Kill()
		http.Error(w, err.Error(), 500)
		return
	}

	agent.cmd = cmd

	go func() {
		err := cmd.Wait()
		agent.mu.Lock()
		agent.cmd = nil
		agent.mu.Unlock()

		if err != nil {
			log.Println("Workload ended:", err)
		}
	}()

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "Workload started")
}

func metrics(w http.ResponseWriter, r *http.Request) {
	agent.mu.Lock()
	defer agent.mu.Unlock()

	usage, err := readUsage()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	now := time.Now()
	elapsed := now.Sub(agent.lastTime).Seconds()

	delta := uint64(0)
	if usage >= agent.previous {
		delta = usage - agent.previous
	}

	utilization := 0.0

	if elapsed > 0 && agent.quota > 0 {
		allocatedCores := float64(agent.quota) / float64(period)
		utilization = float64(delta) / (elapsed * 1000000 * allocatedCores) * 100
	}

	agent.previous = usage
	agent.lastTime = now

	result := Metrics{
		NodeID:      agent.id,
		UsageUsec:   usage,
		Utilization: utilization,
		QuotaCores:  float64(agent.quota) / float64(period),
		MaxCores:    runtime.NumCPU(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func updateQuota(w http.ResponseWriter, r *http.Request) {
	var request QuotaRequest

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()

	if err := setQuota(request.Cores); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	fmt.Fprintf(w, "Node %s quota updated to %.2f cores\n",
		agent.id, float64(agent.quota)/float64(period))
}

func main() {
	if len(os.Args) != 3 {
		log.Fatal("Usage: sudo go run agent.go <node-id> <port>")
	}

	if os.Geteuid() != 0 {
		log.Fatal("Root privileges required")
	}

	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		log.Fatal("Linux cgroup v2 is required")
	}

	agent = &Agent{
		id:       os.Args[1],
		path:     filepath.Join(root, "dca_"+os.Args[1]),
		lastTime: time.Now(),
	}

	if err := os.MkdirAll(agent.path, 0755); err != nil {
		log.Fatal(err)
	}

	if err := setQuota(float64(runtime.NumCPU()) / 2); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/start", startWorkload)
	http.HandleFunc("/metrics", metrics)
	http.HandleFunc("/quota", updateQuota)

	log.Printf("DCA agent %s listening on port %s", agent.id, os.Args[2])
	log.Fatal(http.ListenAndServe(":"+os.Args[2], nil))
}
