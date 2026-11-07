
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"time"
)

type Node struct {
	ID      string
	URL     string
	MaxCore float64
}

type Metrics struct {
	NodeID      string  `json:"node_id"`
	Utilization float64 `json:"utilization_percent"`
	QuotaCores  float64 `json:"quota_cores"`
	MaxCores    int     `json:"max_cores"`
}

type QuotaRequest struct {
	Cores float64 `json:"cores"`
}

var client = &http.Client{Timeout: 5 * time.Second}

func getMetrics(node Node) (Metrics, error) {
	var result Metrics

	response, err := client.Get(node.URL + "/metrics")
	if err != nil {
		return result, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("metrics request returned %s", response.Status)
	}

	err = json.NewDecoder(response.Body).Decode(&result)
	return result, err
}

func updateQuota(node Node, cores float64) error {
	body, err := json.Marshal(QuotaRequest{Cores: cores})
	if err != nil {
		return err
	}

	response, err := client.Post(
		node.URL+"/quota",
		"application/json",
		bytes.NewBuffer(body),
	)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("quota update returned %s", response.Status)
	}

	return nil
}

func startWorkload(node Node) error {
	response, err := client.Post(node.URL+"/start", "application/json", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("workload start returned %s", response.Status)
	}

	return nil
}

func main() {
	threshold := flag.Float64("threshold", 75, "CPU utilization threshold")
	interval := flag.Duration("interval", 2*time.Second, "Monitoring interval")
	duration := flag.Duration("duration", 30*time.Second, "Experiment duration")
	flag.Parse()

	nodes := []Node{
		{ID: "node1", URL: "http://192.168.1.101:8080"},
		{ID: "node2", URL: "http://192.168.1.102:8080"},
		{ID: "node3", URL: "http://192.168.1.103:8080"},
	}

	for i := range nodes {
		if err := startWorkload(nodes[i]); err != nil {
			fmt.Printf("%s workload start failed: %v\n", nodes[i].ID, err)
		}
	}

	deadline := time.Now().Add(*duration)

	for time.Now().Before(deadline) {
		time.Sleep(*interval)

		fmt.Println("\nDYNAMIC CPU ALLOCATION")
		fmt.Println("----------------------------------------------")

		for _, node := range nodes {
			m, err := getMetrics(node)
			if err != nil {
				fmt.Printf("%s monitoring error: %v\n", node.ID, err)
				continue
			}

			newQuota := m.QuotaCores

			if m.Utilization > *threshold {
				newQuota += 0.25
			} else if m.Utilization < *threshold-15 {
				newQuota -= 0.25
			}

			if newQuota < 0.1 {
				newQuota = 0.1
			}

			if newQuota > float64(m.MaxCores) {
				newQuota = float64(m.MaxCores)
			}

			if newQuota != m.QuotaCores {
				if err := updateQuota(node, newQuota); err != nil {
					fmt.Printf("%s quota update failed: %v\n", node.ID, err)
				}
			}

			fmt.Printf(
				"%s | Utilization: %.2f%% | Previous quota: %.2f cores | New quota: %.2f cores\n",
				node.ID,
				m.Utilization,
				m.QuotaCores,
				newQuota,
			)
		}
	}
}
