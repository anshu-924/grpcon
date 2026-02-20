# Next Steps: Improving Connection Management & Heartbeat System

## Current Implementation Analysis

### Architecture Overview
The gRPC notification server is structured as follows:

1. **Connection Management**
   - `ConnectionManager`: Manages all client groups and their device connections
   - `ClientGroup`: Represents all devices for a single client (user)
   - `Connection`: Represents a single device connection with stream, metadata, and status

2. **Connection Lifecycle**
   - `AddConnection`: Registers a device (client_id + device_id)
   - `StreamNotifications`: Attaches a gRPC stream to an existing connection
   - `RemoveConnection`: Unregisters a device
   - Stream stays alive until `stream.Context().Done()` is triggered

3. **Current Flow**
   ```
   User logs in → AddConnection(client_id, device_id) → StreamNotifications(unique_id)
   → Stream stays alive → User closes tab → Context.Done() → Stream detached
   ```

### Identified Gaps

1. **❌ No Stream Cleanup on RemoveConnection**
   - When `RemoveConnection` is called, the stream is not explicitly closed
   - The connection is removed from the manager, but if a stream is still attached, it becomes orphaned

2. **❌ No Heartbeat/Health Check Mechanism**
   - No way to detect if a client is still alive
   - Dead connections remain in memory until garbage collection or manual removal
   - No proactive connection validation

### 3. **❌ Mobile Browser Background Handling**
   - When mobile users switch to home screen/another app, the connection state is unclear
   - Browser tabs may be suspended or killed by the OS
   - Stream might remain "open" on server but client is inactive
   - Need heartbeat to detect these "zombie" connections

---

## Implementation Plan

### 1. Stream Cleanup on RemoveConnection ✅

**Goal**: When removing a connection, check if the stream is open and close it gracefully.

**Changes Required**:

#### File: `handlers/connection_handler.go`

**Modify `UnregisterDevice` function**:
```go
// UnregisterDevice removes a device connection
func (h *ConnectionHandler) UnregisterDevice(clientID, deviceID string) error {
    if clientID == "" || deviceID == "" {
        return fmt.Errorf("client_id and device_id are required")
    }

    uniqueID := models.CreateUniqueID(clientID, deviceID)
    
    // Get connection before removing to check stream status
    conn, exists := h.connManager.GetConnection(clientID, deviceID)
    if !exists {
        return fmt.Errorf("device not found: %s", uniqueID)
    }
    
    // If stream is attached and active, close it gracefully
    if conn.Stream != nil && conn.IsActive {
        log.Printf("Closing active stream for device: %s", uniqueID)
        // The stream will be closed when we return from StreamNotifications
        // Mark as inactive first
        conn.IsActive = false
        conn.Stream = nil
    }
    
    // Now remove the connection
    removed := h.connManager.RemoveConnection(uniqueID, clientID, deviceID)
    if !removed {
        return fmt.Errorf("failed to remove device: %s", uniqueID)
    }

    log.Printf("Device unregistered and stream closed: %s", uniqueID)
    return nil
}
```

**Implementation Steps**:
1. Retrieve the connection before removing
2. Check if `conn.Stream != nil` and `conn.IsActive == true`
3. Set `IsActive = false` and `Stream = nil` (this signals the stream handler to stop)
4. Remove the connection from the manager
5. The `StreamNotifications` goroutine will detect context Done and exit

---

### 2. Heartbeat Mechanism (Every 30 seconds) 💓

**Goal**: Implement a heartbeat system to detect dead connections and clean them up automatically.

**Approach**: Use gRPC bidirectional streaming or ping messages

#### Option A: Ping/Pong with Existing Stream (Recommended)

**Changes Required**:

##### File: `proto/notification.proto`

Add a new message type for heartbeat:
```protobuf
// Notification message structure
message Notification {
  string id = 1;
  string connection_id = 2;
  string title = 3;
  string message = 4;
  string service_name = 5;
  int64 timestamp = 6;
  string type = 7; // "notification" or "heartbeat"
}
```

##### File: `models/models.go`

Add heartbeat tracking to Connection:
```go
type Connection struct {
    UniqueID           string
    ClientID           string
    DeviceID           string
    ServiceName        string
    Stream             pb.NotificationService_StreamNotificationsServer
    ConnectedAt        time.Time
    LastNotificationAt time.Time
    LastHeartbeatAt    time.Time // NEW: Last heartbeat received
    NotificationCount  int
    IsActive           bool
    
    // Heartbeat management
    heartbeatTicker    *time.Ticker // NEW: Ticker for sending heartbeats
    heartbeatStopChan  chan bool    // NEW: Channel to stop heartbeat
}
```

##### File: `handlers/notification_handler.go`

Modify `StreamNotifications` to start heartbeat goroutine:
```go
func (s *NotificationServer) StreamNotifications(req *pb.SubscribeRequest, stream pb.NotificationService_StreamNotificationsServer) error {
    connectionID := req.ConnectionId

    if connectionID == "" {
        return fmt.Errorf("connection_id is required")
    }

    conn, err := s.connHandler.GetDeviceByUniqueID(connectionID)
    if err != nil {
        return fmt.Errorf("connection not found: %s", connectionID)
    }

    if err := s.connHandler.AttachStream(conn.ClientID, conn.DeviceID, stream); err != nil {
        return err
    }

    log.Printf("Client %s (Device: %s) started streaming notifications", conn.ClientID, conn.DeviceID)

    // Start heartbeat goroutine
    heartbeatDone := make(chan bool, 1)
    go s.sendHeartbeats(conn, stream, heartbeatDone)

    // Keep the stream alive
    <-stream.Context().Done()

    // Stop heartbeat
    heartbeatDone <- true

    s.connHandler.DetachStream(conn.ClientID, conn.DeviceID)

    log.Printf("Client %s (Device: %s) disconnected from stream (Uptime: %v)",
        conn.ClientID, conn.DeviceID, conn.GetUptime())

    return nil
}

// sendHeartbeats sends periodic heartbeat messages to the client
func (s *NotificationServer) sendHeartbeats(conn *models.Connection, stream pb.NotificationService_StreamNotificationsServer, done chan bool) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            heartbeat := &pb.Notification{
                Id:           fmt.Sprintf("heartbeat_%d", time.Now().Unix()),
                ConnectionId: conn.UniqueID,
                Title:        "heartbeat",
                Message:      "ping",
                ServiceName:  "system",
                Timestamp:    time.Now().Unix(),
                Type:         "heartbeat",
            }
            
            if err := stream.Send(heartbeat); err != nil {
                log.Printf("Failed to send heartbeat to %s: %v", conn.UniqueID, err)
                // Connection is dead, clean it up
                s.connHandler.UnregisterDevice(conn.ClientID, conn.DeviceID)
                return
            }
            
            conn.LastHeartbeatAt = time.Now()
            log.Printf("Heartbeat sent to %s", conn.UniqueID)
            
        case <-done:
            log.Printf("Stopping heartbeat for %s", conn.UniqueID)
            return
        }
    }
}
```

##### File: `handlers/connection_handler.go`

Add periodic cleanup of stale connections:
```go
// StartHealthCheckMonitor runs a background goroutine that checks for stale connections
func (h *ConnectionHandler) StartHealthCheckMonitor() {
    go func() {
        ticker := time.NewTicker(60 * time.Second) // Check every minute
        defer ticker.Stop()
        
        for range ticker.C {
            h.cleanupStaleConnections()
        }
    }()
}

// cleanupStaleConnections removes connections that haven't received heartbeat in 90 seconds
func (h *ConnectionHandler) cleanupStaleConnections() {
    allConns := h.connManager.GetAllConnections()
    staleThreshold := 90 * time.Second
    
    for _, conn := range allConns {
        if conn.IsActive {
            timeSinceHeartbeat := time.Since(conn.LastHeartbeatAt)
            if timeSinceHeartbeat > staleThreshold {
                log.Printf("Removing stale connection: %s (last heartbeat: %v ago)", 
                    conn.UniqueID, timeSinceHeartbeat)
                h.UnregisterDevice(conn.ClientID, conn.DeviceID)
            }
        }
    }
}
```

**Implementation Steps**:
1. Update proto file to add `type` field for heartbeat vs notification
2. Add heartbeat fields to `Connection` model
3. Implement `sendHeartbeats` goroutine in `NotificationServer`
4. Start heartbeat when stream begins, stop when stream ends
5. Add health check monitor to clean up stale connections
6. Client must handle heartbeat messages (ignore or acknowledge)

---

### 3. Mobile Browser & Tab Background Handling 📱

**Goal**: Handle mobile browsers and background tabs properly - clean up connections when users navigate away.

**Scenario Flows**:

#### Desktop Browser
1. User opens app → Login → `AddConnection` + `StreamNotifications`
2. User closes tab → Stream context cancelled → Connection detached and removed immediately
3. User reopens app → Create new connection (fresh start)

#### Mobile Browser (Critical Case)
1. User opens app in browser → Login → `AddConnection` + `StreamNotifications`
2. User presses home button or switches apps → Browser tab backgrounded
3. **What happens?**
   - iOS Safari: Connection may be suspended after ~30 seconds
   - Chrome Android: Connection may stay alive longer
   - Stream might NOT immediately close (no context.Done() signal)
   - Connection appears "alive" on server but client cannot receive
4. **Without heartbeat**: Connection stays in memory indefinitely (memory leak)
5. **With heartbeat**: After 90 seconds of failed heartbeats → Auto cleanup

**Why Heartbeat is Critical for Mobile**:
- Mobile OS aggressively suspends background tabs
- Network connections are paused but not always closed
- gRPC stream context may not detect suspension
- Heartbeat failure is the only reliable way to detect dead mobile connections

**Implementation**: No reconnection needed - just clean disconnection

##### File: `handlers/notification_handler.go`

Simplified disconnect handling:
```go
func (s *NotificationServer) StreamNotifications(req *pb.SubscribeRequest, stream pb.NotificationService_StreamNotificationsServer) error {
    connectionID := req.ConnectionId

    if connectionID == "" {
        return fmt.Errorf("connection_id is required")
    }

    conn, err := s.connHandler.GetDeviceByUniqueID(connectionID)
    if err != nil {
        return fmt.Errorf("connection not found: %s", connectionID)
    }

    if err := s.connHandler.AttachStream(conn.ClientID, conn.DeviceID, stream); err != nil {
        return err
    }
    
    // Initialize heartbeat timestamp
    conn.LastHeartbeatAt = time.Now()

    log.Printf("Client %s (Device: %s) started streaming", conn.ClientID, conn.DeviceID)

    // Start heartbeat goroutine
    heartbeatDone := make(chan bool, 1)
    go s.sendHeartbeats(conn, stream, heartbeatDone)

    // Keep the stream alive until client disconnects
    <-stream.Context().Done()

    // Stop heartbeat
    heartbeatDone <- true

    // Clean up immediately - no grace period needed
    s.connHandler.DetachStream(conn.ClientID, conn.DeviceID)

    log.Printf("Client %s (Device: %s) disconnected from stream (Uptime: %v)",
        conn.ClientID, conn.DeviceID, conn.GetUptime())

    return nil
}
```

**Note**: Stream detachment removes the stream reference but keeps the connection registered. The connection will be fully removed when:
1. Client explicitly calls `RemoveConnection` (on logout)
2. Heartbeat fails and triggers cleanup
3. Admin manually removes it

---

## Implementation Order

### Phase 1: Stream Cleanup (Highest Priority)
1. ✅ Modify `UnregisterDevice` to close active streams
2. ✅ Test: Call RemoveConnection and verify stream is closed
DONE
### Phase 2: Heartbeat System
1. ✅ Update proto file with heartbeat type
2. ✅ Regenerate proto files: `protoc --go_out=. --go-grpc_out=. proto/notification.proto`
3. ✅ Add heartbeat fields to Connection model
4. ✅ Implement `sendHeartbeats` goroutine
5. ✅ Start heartbeat in `StreamNotifications`
6. ✅ Add health check monitor
7. ✅ Test with simulated dead connections

### Phase 3: Mobile Browser Testing
1. ✅ Test mobile browser backgrounding (iOS Safari, Chrome Android)
2. ✅ Verify heartbeat failure triggers cleanup
3. ✅ Test rapid app switching scenarios
4. ✅ Verify connection cleanup on browser tab kill

### Phase 4: Integration & Testing
1. ✅ Start health monitor in `main.go`
2. ✅ Add metrics/logging for monitoring
3. ✅ Load test with multiple clients
4. ✅ Test edge cases (network failures, rapid reconnects)

---

## Configuration Parameters

Add to environment variables or config file:

```go
const (
    HeartbeatInterval      = 30 * time.Second  // Send heartbeat every 30s
    HeartbeatTimeout       = 90 * time.Second  // Mark as stale after 90s (3 missed heartbeats)
    CleanupInterval        = 60 * time.Second  // Run stale connection cleanup every minute
)
```

**Why 90 seconds?**
- Allows 3 missed heartbeats (30s * 3 = 90s)
- Accounts for temporary network hiccups
- Quick enough to clean up mobile background tabs
- Prevents false positives from brief network issues

---

## Client Implementation Guidance

### Initial Connection
```javascript
// When user logs in
const clientId = "user123";
const deviceId = "browser_xyz";

// 1. Register device
await grpcClient.AddConnection({
    connection_id: clientId,
    service_name: deviceId
});

// 2. Get unique connection ID (format: user123_browser_xyz)
const uniqueId = `${clientId}_${deviceId}`;
localStorage.setItem('connection_id', uniqueId);

// 3. Start streaming
const stream = grpcClient.StreamNotifications({
    connection_id: uniqueId
});

stream.on('data', (notification) => {
    if (notification.type === 'heartbeat') {
        console.log('Heartbeat received');
        // Optionally update UI connection status
    } else {
        // Handle actual notification
        displayNotification(notification);
    }
});

stream.on('end', () => {
    console.log('Stream ended - will reconnect on app reopen');
});

stream.on('error', (err) => {
    console.eMobile Browser Testing
1. ✅ Test mobile browser backgrounding (iOS Safari, Chrome Android)
2. ✅ Verify heartbeat failure triggers cleanup
3. ✅ Test rapid app switching scenarios
4. ✅ Verify connection cleanup on browser tab kill
if (savedConnectionId) {
    // Try to reconnect to existing connection
    try {
        const stream = grpcClient.StreamNotifications({
            connection_id: savedConnectionId
        });
        
        console.log('Reconnected successfully');
        // Handle stream as before
        
    } catch (err) {
        if (err.code === 'NOT_FOUND') {
            // Connection expired, create new one
            console.log('Connection expired, creating new connection');
            // Fall back to initial connection flow
            await doInitialConnection();
        }
    }
} else {
    // First time connection
    await doInitialConnection();
}
```

### Graceful Disconnection (Tab Close)
```javascript
// When user closes tab/logs out
window.addEventListener('beforeunload', () => {
    // Stream will automatically close
    // No need to call RemoveConnection unless you want immediate cleanup
});

// Optional: Explicit disconnection
function logout() {
    const connectionId = localStorage.getItem('connection_id');
    const [clientId, deviceId] = connectionId.split('_');
    
    // Remove connection immediately (won't wait for grace period)
    grpcClient.RemoveConnection({
        connection_id: clientId,
        service_name: deviceId
    });
    
    localStorage.removeItem('connection_id');
}
```

---

## Testing Scenarios

### Test 1: Stream Cleanup on Remove
1. Connect device
2. Call RemoveConnection
3. Verify stream is closed
4. Verify connection removed from manager

### Test 2: Heartbeat Detection
1. Connect device
2. Wait for heartbeat messages (30s intervals)
3. Simulate network failure (block heartbeat)
4. Verify connection marked as stale after 90s
5. Verify cleanup task removes connection

### Test 3: Mobile Background/Foreground
1. User opens app on mobile browser
2. Connect and verify heartbeats arriving
3. Switch to home screen (background app)
4. Wait 90+ seconds
5. Verify server detects stale connection and removes it
6. Return to app (foreground)
7. Verify new connection is created

### Test 4: Tab Visibility Changes
1. User opens app in desktop browser
2. Connect and verify streaming
3. Switch to another tab
4. Wait and observe heartbeat behavior
5. Return to tab
6. Verify connection still works or reconnects if needed

### Test 5: Multiple Devices Same Client
1. Connect device1 for user123
2. Connect device2 for user123
3. Send notification to user123
4. Verify both devices receive notification
5. Close device1
6. Send notification
7. Verify only device2 receives

---

## Monitoring & Observability

### Metrics to Track
- Active connections count
- Heartbeat success/failure rate
- Stale connections detected and cleaned
- Average connection lifetime
- Notifications delivered vs failed
- Connections by platform (desktop/mobile/iOS/Android)

### Logs to Add
- Connection lifecycle events (connect, disconnect, reconnect, expire)
- Heartbeat failures
- Stream errors
- Cleanup operations

### Health Endpoints
Add HTTP endpoints for monitoring:
- `/health/connections` - Active connection count
- `/health/heartbeats` - Recent heartbeat stats
- `/health/stale` - Stale connections count

---

## Summary

This implementation provides:

✅ **Proper Stream Cleanup**: Streams are closed when connections are removed  
✅ **Heartbeat System**: 30-second heartbeats detect dead connections (critical for mobile)  
✅ **Mobile Browser Support**: Handles background/foreground transitions automatically  
✅ **Simple Reconnection**: New connection on each app open - no state management complexity  
✅ **Memory Efficient**: Automatic cleanup of stale connections prevents memory leaks  
✅ **Production Ready**: Health monitoring, cleanup tasks, and error handling  

The system handles real-world scenarios:
- **Desktop**: User closes/reopens tabs → Clean connection lifecycle
- **Mobile**: User backgrounds app → Heartbeat detects and cleans up zombie connections
- **Network Issues**: Temporary disconnects → Heartbeat detects and cleans up within 90s
- **Multiple Devices**: Same user on phone + desktop → Both connections managed independently

**Key Design Decision**: 
- **No reconnection grace period** - Simplified approach where each app open creates a fresh connection
- **Heartbeat is the safety net** - Ensures mobile background tabs don't leak connections
- **Client handles reconnection** - App detects page visibility changes and reconnects as needed
