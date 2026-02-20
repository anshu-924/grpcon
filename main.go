package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"grpcon/models"
	"grpcon/services"
)

func main() {
	// Default ports
	grpcPort := ":50051"
	httpPort := ":8080"

	if p := os.Getenv("GRPC_PORT"); p != "" {
		grpcPort = ":" + p
	}
	if p := os.Getenv("HTTP_PORT"); p != "" {
		httpPort = ":" + p
	}

	// Create and start gRPC server
	server, err := services.NewServer(grpcPort)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	// Start health check monitor for stale connections
	notifServer := server.GetNotificationServer()
	connHandler := notifServer.GetConnectionHandler()
	connHandler.StartHealthCheckMonitor()
	log.Println("Health check monitor started successfully")

	// Start HTTP gateway using the SAME notification server
	go func() {
		log.Printf("Starting HTTP gateway on %s", httpPort)
		setupHTTPGateway(server, httpPort)
	}()

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Received shutdown signal")
		server.Stop()
		os.Exit(0)
	}()

	// Start serving gRPC
	log.Printf("gRPC server listening on %s", grpcPort)
	if err := server.Start(); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}

func setupHTTPGateway(server *services.Server, port string) {
	notifServer := server.GetNotificationServer()

	http.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]string{"error": "Only POST method allowed"})
			return
		}

		var req struct {
			ClientID string `json:"client_id"`
			Title    string `json:"title"`
			Message  string `json:"message"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
			return
		}

		notification := &models.NotificationData{
			ID:          fmt.Sprintf("notif_%d", time.Now().Unix()),
			ClientID:    req.ClientID,
			Title:       req.Title,
			Message:     req.Message,
			ServiceName: "http_gateway",
			Timestamp:   time.Now().Unix(),
		}
		//change here to send to all devices of the client instead of only first device
		// err := notifServer.SendNotificationToClient(notification)
		err:=notifServer.GetConnectionHandler().SendToFirstDevice(notification)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		json.NewEncoder(w).Encode(map[string]string{"status": "sent"})
	})

	// Get connection stats endpoint
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		stats := notifServer.GetConnectionStats()
		json.NewEncoder(w).Encode(stats)
	})

	// List all clients endpoint
	http.HandleFunc("/clients", func(w http.ResponseWriter, r *http.Request) {
		connHandler := notifServer.GetConnectionHandler()
		clientIDs := connHandler.GetConnectionManager().GetAllClientIDs()

		clientsInfo := make(map[string]interface{})
		for _, clientID := range clientIDs {
			devices, _ := connHandler.GetClientDevices(clientID)
			deviceList := make([]map[string]interface{}, 0)
			for _, device := range devices {
				deviceList = append(deviceList, map[string]interface{}{
					"device_id":    device.DeviceID,
					"unique_id":    device.UniqueID,
					"service_name": device.ServiceName,
					"is_active":    device.IsActive,
					"connected_at": device.ConnectedAt,
					"notif_count":  device.NotificationCount,
				})
			}
			clientsInfo[clientID] = deviceList
		}

		json.NewEncoder(w).Encode(clientsInfo)
	})

	log.Fatal(http.ListenAndServe(port, nil))
}
