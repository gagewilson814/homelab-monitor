package main

import (
	"encoding/json"
	"fmt"
	"homeserver-monitor/internal/stats"
	"log"
	"net/http"
	"time"
)

var serverAddresses = [2]string{
	"localhost:8080", // placeholder
	"localhost:8081", // placeholder
}

type AgentResponse struct {
	Address string
	Data    stats.Response
	Err     error
}

func poll(server string) AgentResponse {

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	response, err := client.Get("http://" + server + "/stats")
	if err != nil {
		log.Println("Error polling server: ", err)
		return AgentResponse{Address: server, Err: err}
	}

	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		err = fmt.Errorf("unexpected status: %s", response.Status)
		log.Println("Error polling server: ", err)
		return AgentResponse{Address: server, Err: err}
	}

	var statsResponse stats.Response
	err = json.NewDecoder(response.Body).Decode(&statsResponse)
	if err != nil {
		log.Println("Error decoding response from server ", server, ": ", err)
		return AgentResponse{Address: server, Err: err}
	}

	return AgentResponse{Address: server, Data: statsResponse}
}

func main() {
	for _, server := range serverAddresses {
		result := poll(server)
		if result.Err != nil {
			log.Println("Error polling server ", server, ": ", result.Err)
			continue
		}
		log.Printf("Stats from server %s: %+v\n", result.Address, result.Data)
	}
}
