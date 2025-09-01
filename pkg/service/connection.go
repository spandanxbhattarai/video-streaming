package service

import (
	"context"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/livekit"
)

type ConnectionStore interface {
	// Active Connections
	AddActiveConnection(ctx context.Context, roomName livekit.RoomName, userID string)
	RemoveActiveConnection(ctx context.Context, roomName livekit.RoomName, userID string)
	ListActiveConnections(ctx context.Context, roomName livekit.RoomName) []string
	
	// Pending Connections
	AddPendingConnection(ctx context.Context, roomName livekit.RoomName, req *PendingRequest)
	RemovePendingConnection(ctx context.Context, roomName livekit.RoomName, requestID string)
	ListPendingConnections(ctx context.Context, roomName livekit.RoomName) []*PendingRequest
	ListAllPendingConnections(ctx context.Context) []*PendingRequest // NEW METHOD
	GetPendingConnection(ctx context.Context, roomName livekit.RoomName, requestID string) (*PendingRequest, error)
	GetPendingConnectionByID(ctx context.Context, requestID string) (*PendingRequest, error)
	
	// Admin Connections
	AddAdminConnection(ctx context.Context, roomName livekit.RoomName, adminID string, conn *websocket.Conn)
	GetAdminConnection(ctx context.Context, adminID string) *websocket.Conn
	ListAdminConnections(ctx context.Context, roomName livekit.RoomName) []*websocket.Conn
	RemoveAdminConnection(ctx context.Context, adminID string)
	RemoveAdminConnectionByConn(ctx context.Context, conn *websocket.Conn) (bool, livekit.RoomName, string)
	
	// Cleanup
	CleanupRoom(ctx context.Context, roomName livekit.RoomName) bool
}