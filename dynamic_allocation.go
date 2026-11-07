
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
	period     = int64(100000)
	minQuota   = int64(10000)
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
	path := filepath.Join(cgroupRoot, fmt.Sprintf("dca_node_%d", id))

	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, err
	}

	node := &Node{ID: id, Path: path}

	if err := setQuota(node, quota); err != nil {
		return nil, err
	}

	return node, nil
}

func setQuota(node *Node, quota int64) error {
	if quota < minQuota {
		quota = minQuota
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

func startWorkload(node *Node, duration time.Duration, wg *sync.WaitGroup) error {
	cmd := exec.Command(os.Args[0], "worker", duration.String())

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

func allocateNodes(nodes []*Node) error {
	totalQuota := int64(runtime.NumCPU()) * period
	reservedQuota := int64(len(nodes)) * minQuota
	availableQuota := totalQuota - reservedQuota

	if availableQuota < 0 {
		return fmt.Errorf("insufficient CPU capacity for %d nodes", len(nodes))
	}

	totalUsage := uint64(0)

	for _, node := range nodes {
		totalUsage += node.Usage - node.Previous
	}

	if totalUsage == 0 {
		return nil
	}

	for _, node := range nodes {
		usageDelta := node.Usage - node.Previous
		share := float64(usageDelta) / float64(totalUsage)

		newQuota := minQuota + int64(float64(availableQuota)*share)

		if err := setQuota(node, newQuota); err != nil {
			return err
		}
	}

	return nil
}

func monitorAndAllocate(nodes []*Node, duration time.Duration) {
	start := time.Now()

	for time.Since(start) < duration {
		time.Sleep(2 * time.Second)

		for _, node := range nodes {
			usage, err := readCPUUsage(node)

			if err != nil {
				fmt.Printf("Node %d measurement error: %v\n", node.ID, err)
				continue
			}

			node.Previous = node.Usage
			node.Usage = usage
		}

		if err := allocateNodes(nodes); err != nil {
			fmt.Println("Allocation error:", err)
			continue
		}

		fmt.Println("\nDYNAMIC CPU ALLOCATION UPDATE")
		fmt.Println("---------------------------------------------")

		for _, node := range nodes {
			fmt.Printf(
				"Node %d | CPU quota: %.2f cores | CPU usage: %d usec\n",
				node.ID,
				float64(node.Quota)/float64(period),
				node.Usage,
			)
		}
	}
}

func report(nodes []*Node, duration time.Duration) {
	fmt.Println("\nFINAL CPU UTILIZATION")
	fmt.Println("---------------------------------------------")

	for _, node := range nodes {
		usage, err := readCPUUsage(node)

		if err != nil {
			fmt.Printf("Node %d measurement error: %v\n", node.ID, err)
			continue
		}

		cpuSeconds := float64(usage) / 1000000
		elapsed := duration.Seconds()
		allocatedCores := float64(node.Quota) / float64(period)

		utilization := 0.0

		if elapsed > 0 && allocatedCores > 0 {
			utilization = cpuSeconds / (elapsed * allocatedCores) * 100
		}

		fmt.Printf(
			"Node %d | CPU Utilization: %.2f%%\n",
			node.ID,
			utilization,
		)
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		duration, err := time.ParseDuration(os.Args[2])
		if err != nil {
			os.Exit(1)
		}
		runWorker(duration)
		return
	}

	if len(os.Args) < 2 {
		fmt.Println("Usage: sudo go run dca.go <node-count> [duration]")
		os.Exit(1)
	}

	count, err := strconv.Atoi(os.Args[1])
	if err != nil || count < 3 {
		fmt.Println("Use a valid node count: 3, 5, 7, 9 or 11")
		os.Exit(1)
	}

	if count != 3 && count != 5 && count != 7 && count != 9 && count != 11 {
		fmt.Println("Supported node counts: 3, 5, 7, 9, 11")
		os.Exit(1)
	}

	duration := 30 * time.Second

	if len(os.Args) > 2 {
		duration, err = time.ParseDuration(os.Args[2])
		if err != nil || duration <= 0 {
			fmt.Println("Invalid duration")
			os.Exit(1)
		}
	}

	if os.Geteuid() != 0 {
		fmt.Println("Run with root privileges")
		os.Exit(1)
	}

	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		fmt.Println("Linux cgroup v2 is required")
		os.Exit(1)
	}

	totalQuota := int64(runtime.NumCPU()) * period
	initialQuota := totalQuota / int64(count)

	if initialQuota < minQuota {
		fmt.Println("Insufficient CPU capacity for requested nodes")
		os.Exit(1)
	}

	nodes := make([]*Node, 0, count)

	for i := 1; i <= count; i++ {
		node, err := createNode(i, initialQuota)
		if err != nil {
			fmt.Println("Node initialization failed:", err)
			os.Exit(1)
		}
		nodes = append(nodes, node)
	}

	var wg sync.WaitGroup

	for _, node := range nodes {
		if err := startWorkload(node, duration, &wg); err != nil {
			fmt.Printf("Node %d workload error: %v\n", node.ID, err)
		}
	}

	monitorAndAllocate(nodes, duration)
	wg.Wait()
	report(nodes, duration)

	for _, node := range nodes {
		os.RemoveAll(node.Path)
	}
}
