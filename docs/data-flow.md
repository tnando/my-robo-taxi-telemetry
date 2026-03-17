# End-to-End Data Flow

## Overview

This document traces a single telemetry data point from a Tesla vehicle through the entire system to a browser rendering a map update.

## Flow Diagram

```
Time ──────────────────────────────────────────────────────────────────►

Tesla Vehicle                Telemetry Server                    Browser
─────────────               ─────────────────                   ────────

1. Speed changes
   to 65 mph

2. Vehicle batches
   data (500ms window)

3. Sends protobuf
   over mTLS WSS ──────────► 4. Receiver decodes
                                protobuf payload

                             5. Publishes VehicleTelemetryEvent
                                to event bus

                             6. Fan-out to subscribers:

                             6a. Drive Detector:
                                 - Updates route point
                                 - Recalculates max speed
                                 - Publishes DriveUpdatedEvent

                             6b. Store:
                                 - Batch-writes vehicle
                                   status to PostgreSQL
                                 - Appends route point
                                   to active drive

                             6c. WS Broadcast:
                                 - Maps Tesla fields to
                                   frontend field names
                                 - Looks up vehicleId
                                   (DB ID, not VIN)
                                 - Builds JSON message ──────► 7. useVehicleStream
                                                                  receives message

                                                               8. Merges fields into
                                                                  Vehicle state map

                                                               9. React re-renders:
                                                                  - Map marker moves
                                                                  - Speed display: 65
                                                                  - Trip progress updates
```

## Detailed Steps

### Step 1-3: Vehicle → Server (Tesla Protocol)

**Trigger:** A telemetry field value changes AND the configured interval has elapsed.

**Protocol:**
- Transport: WebSocket over mTLS (port 443)
- Authentication: Vehicle presents Tesla-issued client certificate
- Serialization: Protocol Buffers (`vehicle_data.proto`)
- Batching: Vehicle collects changes in 500ms windows

**Payload (decoded to JSON for illustration):**
```json
{
  "data": [
    {"key": "VehicleSpeed", "value": {"stringValue": "65.2"}},
    {"key": "Location", "value": {"locationValue": {"latitude": 33.0903, "longitude": -96.8237}}},
    {"key": "Heading", "value": {"stringValue": "245"}}
  ],
  "createdAt": "2026-03-16T14:30:00.314Z",
  "vin": "5YJ3E7EB2NF000001"
}
```

### Step 4: Protobuf Decoding

**Component:** `internal/telemetry/Receiver`

1. Extract VIN from mTLS client certificate (never trust payload VIN alone)
2. Decode protobuf binary → `TelemetryPayload` struct
3. Normalize values:
   - `stringValue` "65.2" → `float64(65.2)`
   - `locationValue` → `Location{Lat: 33.0903, Lng: -96.8237}`
   - Validate ranges (speed 0-250, lat -90/90, lng -180/180)
4. Attach metadata: received timestamp, connection ID

### Step 5: Event Publication

**Component:** `internal/events/Bus`

```go
bus.Publish(ctx, "telemetry.vehicle", events.Event{
    ID:        ulid.Make().String(),
    Topic:     "telemetry.vehicle",
    Timestamp: payload.Timestamp,
    Payload: VehicleTelemetryEvent{
        VIN:       "5YJ3E7EB2NF000001",
        VehicleID: "clx_abc123",  // looked up from VIN→vehicleID cache
        Fields: map[string]any{
            "VehicleSpeed": 65.2,
            "Location":     Location{Lat: 33.0903, Lng: -96.8237},
            "Heading":      245,
        },
        Timestamp: payload.Timestamp,
    },
})
```

### Step 6a: Drive Detection

**Component:** `internal/drives/Detector`

```
Receives VehicleTelemetryEvent
  → Looks up VehicleDriveState for VIN
  → State is "Driving" (Gear was D from earlier event)
  → Appends route point: {lat, lng, timestamp, speed}
  → Updates max speed if 65.2 > current max
  → Publishes DriveUpdatedEvent with new route point
```

### Step 6b: Database Persistence

**Component:** `internal/store/VehicleRepo`

```sql
-- Batch vehicle status update (every 5s or 100 events, whichever first)
UPDATE "Vehicle"
SET speed = 65.2,
    latitude = 33.0903,
    longitude = -96.8237,
    heading = 245,
    "lastUpdated" = NOW()
WHERE "teslaVehicleId" = '...'

-- Append route point to active drive
UPDATE "Drive"
SET "routePoints" = "routePoints" || '[{"lat":33.0903,"lng":-96.8237,"timestamp":"...","speed":65.2}]'::jsonb
WHERE "vehicleId" = 'clx_abc123'
  AND "endTime" = ''
```

### Step 6c: WebSocket Broadcast

**Component:** `internal/ws/Hub`

```go
// Transform telemetry event → client message
msg := ClientMessage{
    Type:      "vehicle_update",
    VehicleID: "clx_abc123",  // database ID, NOT VIN
    Fields: map[string]any{
        "speed":     65.2,
        "latitude":  33.0903,
        "longitude": -96.8237,
        "heading":   245,
    },
    Timestamp: event.Timestamp.Format(time.RFC3339),
}

// Find all clients authorized for this vehicle
// Send to each client's buffered channel
for _, client := range hub.clientsForVehicle("clx_abc123") {
    select {
    case client.send <- msgBytes:
        // delivered
    default:
        // client buffer full — drop oldest, log warning
    }
}
```

### Step 7-9: Browser Update

**Component:** `useVehicleStream` hook (MyRoboTaxi Next.js app)

```typescript
// Message received via WebSocket
{
  "type": "vehicle_update",
  "vehicleId": "clx_abc123",
  "fields": {
    "speed": 65.2,
    "latitude": 33.0903,
    "longitude": -96.8237,
    "heading": 245
  },
  "timestamp": "2026-03-16T14:30:00Z"
}

// Hook merges into Vehicle state:
setVehicles(prev => {
  const updated = new Map(prev);
  const vehicle = updated.get("clx_abc123");
  if (vehicle) {
    updated.set("clx_abc123", { ...vehicle, ...fields });
  }
  return updated;
});

// React re-renders components that depend on this vehicle's state
// Map marker moves → speed display updates → trip progress recalculates
```

## Latency Budget

| Segment | Target | Notes |
|---------|--------|-------|
| Vehicle batching | ~500ms | Tesla's 500ms batch window |
| Network (vehicle → server) | ~50ms | Cellular latency |
| Protobuf decode + event publish | < 1ms | In-memory, no I/O |
| Event bus fan-out | < 1ms | Channel send |
| DB batch write | < 10ms | Async, doesn't block broadcast |
| WS broadcast (server → browser) | < 5ms | JSON serialize + send |
| React state update + render | < 16ms | Single frame (60fps) |
| **Total end-to-end** | **~600ms** | Vehicle change → pixel update |

## Failure Modes

| Failure | Impact | Mitigation |
|---------|--------|------------|
| Vehicle disconnects | No telemetry for that vehicle | ConnectivityEvent → UI shows "offline" |
| DB write fails | Stale data in DB, but WebSocket still works | Retry with backoff, log error |
| WebSocket client disconnects | Client stops receiving | Auto-reconnect in frontend (useVehicleStream) |
| Event bus full (backpressure) | Events dropped for slow subscribers | Drop-oldest policy, Prometheus counter |
| Protobuf decode error | Single message lost | Log error, skip message, continue |
| Auth token expired | Client disconnected | Frontend refreshes token, reconnects |
