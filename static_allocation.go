
package main

import (
	"bufio"
	"fmt"
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
	cgroupRoot = "/sys/fs/cgroup"
	period     = 100000
)

type Node struct {
	ID       int
	Path     string
	Quota    int64
	Usage    uint64
	Previous uint64
}

func writeFile(path, value string) error {
	return os.WriteFile(path, []byte(value), 0644)
}

func createNode(id int, quota int64) (*Node, error) {
	path := filepath.Join(cgroupRoot, fmt.Sprintf("cpu_research_node_%d", id))

	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, err
	}

	node := &Node{
		ID:    id,
		Path:  path,
		Quota: quota,
	}

	if err := setQuota(node, quota); err != nil {
		return nil, err
	}

	return node, nil
}

func setQuota(node *Node, quota int64) error {
	if quota < 10000 {
		quota = 10000
	}

	node.Quota = quota

	return writeFile(
		filepath.Join(node.Path, "cpu.max"),
		fmt.Sprintf("%d %d", quota, period),
	)
}

func readCPUUsage(node *Node) (uint64, error) {
	file, err := os.Open(filepath.Join(node.Path, "cpu.stat"))
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

func startWorker(node *Node, duration time.Duration, wg *sync.WaitGroup) error {
	cmd := exec.Command(os.Args[0], "worker", duration.String())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	if err := writeFile(
		filepath.Join(node.Path, "cgroup.procs"),
		strconv.Itoa(cmd.Process.Pid),
	); err != nil {
		cmd.Process.Kill()
		return err
	}

	wg.Add(1)

	go func() {
		defer wg.Done()
		cmd.Wait()
	}()

	return nil
}

func runWorker(duration time.Duration) {
	end := time.Now().Add(duration)

	var value uint64

	for time.Now().Before(end) {
		value++
		if value == 0 {
			value = 1
		}
	}
}

func initializeNodes(count int) ([]*Node, error) {
	totalQuota := int64(runtime.NumCPU()) * period
	quotaPerNode := totalQuota / int64(count)

	nodes := make([]*Node, 0, count)

	for i := 1; i <= count; i++ {
		node, err := createNode(i, quotaPerNode)
		if err != nil {
			return nil, err
		}

		nodes = append(nodes, node)
	}

	return nodes, nil
}

func measureNodes(nodes []*Node, interval time.Duration) {
	time.Sleep(interval)

	for _, node := range nodes {
		usage, err := readCPUUsage(node)
		if err != nil {
			fmt.Printf("Node %d: measurement error: %v\n", node.ID, err)
			continue
		}

		node.Usage = usage
	}
}

func reportCPUUtilization(nodes []*Node, interval time.Duration) {
	fmt.Println("\nCPU UTILIZATION REPORT")
	fmt.Println("------------------------------------------------")

	for _, node := range nodes {
		cpuSeconds := float64(node.Usage) / 1000000
		elapsedSeconds := interval.Seconds()
		allocatedCores := float64(node.Quota) / float64(period)

		utilization := 0.0

		if allocatedCores > 0 && elapsedSeconds > 0 {
			utilization = cpuSeconds / (elapsedSeconds * allocatedCores) * 100
		}

		fmt.Printf(
			"Node %d | CPU Quota: %.2f cores | CPU Utilization: %.2f%%\n",
			node.ID,
			allocatedCores,
			utilization,
		)
	}

	fmt.Println("------------------------------------------------")
}

func runStatic(nodes []*Node, duration time.Duration) {
	fmt.Println("\nSTATIC CPU ALLOCATION")

	var wg sync.WaitGroup

	for _, node := range nodes {
		if err := startWorker(node, duration, &wg); err != nil {
			fmt.Printf("Node %d: %v\n", node.ID, err)
		}
	}

	wg.Wait()

	measureNodes(nodes, 100*time.Millisecond)
	reportCPUUtilization(nodes, duration)
}

func runDynamic(nodes []*Node, duration time.Duration) {
	fmt.Println("\nDYNAMIC CPU ALLOCATION")

	var wg sync.WaitGroup

	for _, node := range nodes {
		if err := startWorker(node, duration, &wg); err != nil {
			fmt.Printf("Node %d: %v\n", node.ID, err)
		}
	}

	start := time.Now()
	previousTime := start

	for time.Since(start) < duration {
		time.Sleep(2 * time.Second)

		now := time.Now()
		interval := now.Sub(previousTime).Seconds()
		previousTime = now

		totalUsage := uint64(0)

		for _, node := range nodes {
			usage, err := readCPUUsage(node)
			if err != nil {
				continue
			}

			node.Previous = node.Usage
			node.Usage = usage
			totalUsage += usage - node.Previous
		}

		if totalUsage == 0 {
			continue
		}

		totalQuota := int64(runtime.NumCPU()) * period
		minimumQuota := int64(10000)
		availableQuota := totalQuota - int64(len(nodes))*minimumQuota

		for _, node := range nodes {
			usageDelta := node.Usage - node.Previous
			share := float64(usageDelta) / float64(totalUsage)

			newQuota := minimumQuota + int64(float64(availableQuota)*share)

			if newQuota > totalQuota {
				newQuota = totalQuota
			}

			if err := setQuota(node, newQuota); err != nil {
				fmt.Printf("Node %d quota update error: %v\n", node.ID, err)
			}

			fmt.Printf(
				"Node %d | CPU Usage: %.2f%% | Updated Quota: %.2f cores | Interval: %.2fs\n",
				node.ID,
				share*100,
				float64(node.Quota)/float64(period),
				interval,
			)
		}
	}

	wg.Wait()

	measureNodes(nodes, 100*time.Millisecond)
	reportCPUUtilization(nodes, duration)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		duration, err := time.ParseDuration(os.Args[2])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		runWorker(duration)
		return
	}

	if len(os.Args) < 3 {
		fmt.Println("Usage: sudo go run main.go <static|dynamic> <node-count> [duration]")
		os.Exit(1)
	}

	mode := strings.ToLower(os.Args[1])

	count, err := strconv.Atoi(os.Args[2])
	if err != nil || count < 3 {
		fmt.Println("Node count must be at least 3")
		os.Exit(1)
	}

	if count != 3 && count != 5 && count != 7 && count != 9 && count != 11 {
		fmt.Println("Supported node counts: 3, 5, 7, 9, 11")
		os.Exit(1)
	}

	duration := 30 * time.Second

	if len(os.Args) > 3 {
		duration, err = time.ParseDuration(os.Args[3])
		if err != nil || duration <= 0 {
			fmt.Println("Invalid duration")
			os.Exit(1)
		}
	}

	if os.Geteuid() != 0 {
		fmt.Println("Run with root privileges to configure cgroup CPU limits")
		os.Exit(1)
	}

	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		fmt.Println("Linux cgroup v2 is required")
		os.Exit(1)
	}

	nodes, err := initializeNodes(count)
	if err != nil {
		fmt.Println("Node initialization failed:", err)
		os.Exit(1)
	}

	switch mode {
	case "static":
		runStatic(nodes, duration)
	case "dynamic":
		runDynamic(nodes, duration)
	default:
		fmt.Println("Mode must be static or dynamic")
		os.Exit(1)
	}

	for _, node := range nodes {
		os.RemoveAll(node.Path)
	}
}
