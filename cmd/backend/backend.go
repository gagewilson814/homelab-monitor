package main

import (
	"encoding/json"
	"fmt"
	"homeserver-monitor/internal/stats"
	"net/http"
	"time"
)

var serverAddresses = []string{
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
		return AgentResponse{Address: server, Err: err}
	}

	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		err = fmt.Errorf("unexpected status: %s", response.Status)
		return AgentResponse{Address: server, Err: err}
	}

	var statsResponse stats.Response
	err = json.NewDecoder(response.Body).Decode(&statsResponse)
	if err != nil {
		return AgentResponse{Address: server, Err: err}
	}

	return AgentResponse{Address: server, Data: statsResponse}
}

func main() {
	var responses = []AgentResponse{}
	for _, server := range serverAddresses {
		result := poll(server)
		responses = append(responses, result)
	}
	fmt.Printf("%+v\n", responses)
}
