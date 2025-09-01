package service

import (
	"context"
	"errors"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/livekit"
)

type LocalConnectionStore struct {
	activeConnections  map[livekit.RoomName]map[string]struct{}
	pendingConnections map[livekit.RoomName]map[string]*PendingRequest
	adminConnections   map[livekit.RoomName]map[string]*websocket.Conn
	adminReverseLookup map[*websocket.Conn]string
	lock               sync.RWMutex
}

func NewLocalConnectionStore() *LocalConnectionStore {
	return &LocalConnectionStore{
		activeConnections:  make(map[livekit.RoomName]map[string]struct{}),
		pendingConnections: make(map[livekit.RoomName]map[string]*PendingRequest),
		adminConnections:   make(map[livekit.RoomName]map[string]*websocket.Conn),
		adminReverseLookup: make(map[*websocket.Conn]string),
	}
}

func (s *LocalConnectionStore) RemoveAdminConnectionByConn(ctx context.Context, conn *websocket.Conn) (bool, livekit.RoomName, string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	
	for room, connections := range s.adminConnections {
		for adminID, adminConn := range connections {
			if adminConn == conn {
				delete(connections, adminID)
				if len(connections) == 0 {
					delete(s.adminConnections, room)
				}
				return true, room, adminID
			}
		}
	}
	return false, "", ""
}

// --- Active Connections ---
func (s *LocalConnectionStore) AddActiveConnection(ctx context.Context, roomName livekit.RoomName, userID string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	
	if s.activeConnections[roomName] == nil {
		s.activeConnections[roomName] = make(map[string]struct{})
	}
	s.activeConnections[roomName][userID] = struct{}{}
}

func (s *LocalConnectionStore) RemoveActiveConnection(ctx context.Context, roomName livekit.RoomName, userID string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	
	if conns := s.activeConnections[roomName]; conns != nil {
		delete(conns, userID)
		if len(conns) == 0 {
			delete(s.activeConnections, roomName)
		}
	}
}

func (s *LocalConnectionStore) ListActiveConnections(ctx context.Context, roomName livekit.RoomName) []string {
	s.lock.RLock()
	defer s.lock.RUnlock()
	
	var list []string
	if conns := s.activeConnections[roomName]; conns != nil {
		for id := range conns {
			list = append(list, id)
		}
	}
	return list
}
func (s *LocalConnectionStore) ListAllPendingConnections(ctx context.Context) []*PendingRequest {
	s.lock.RLock()
	defer s.lock.RUnlock()
	
	var list []*PendingRequest
	for _, reqs := range s.pendingConnections {
		for _, req := range reqs {
			list = append(list, req)
		}
	}
	return list
}
// --- Pending Connections ---
func (s *LocalConnectionStore) AddPendingConnection(ctx context.Context, roomName livekit.RoomName, req *PendingRequest) {
	s.lock.Lock()
	defer s.lock.Unlock()
	
	if s.pendingConnections[roomName] == nil {
		s.pendingConnections[roomName] = make(map[string]*PendingRequest)
	}
	s.pendingConnections[roomName][req.ID] = req
}

func (s *LocalConnectionStore) RemovePendingConnection(ctx context.Context, roomName livekit.RoomName, requestID string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	
	if reqs := s.pendingConnections[roomName]; reqs != nil {
		delete(reqs, requestID)
		if len(reqs) == 0 {
			delete(s.pendingConnections, roomName)
		}
	}
}

func (s *LocalConnectionStore) ListPendingConnections(ctx context.Context, roomName livekit.RoomName) []*PendingRequest {
	s.lock.RLock()
	defer s.lock.RUnlock()
	
	var list []*PendingRequest
	if reqs := s.pendingConnections[roomName]; reqs != nil {
		for _, req := range reqs {
			list = append(list, req)
		}
	}
	return list
}

func (s *LocalConnectionStore) GetPendingConnection(ctx context.Context, roomName livekit.RoomName, requestID string) (*PendingRequest, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	
	if reqs := s.pendingConnections[roomName]; reqs != nil {
		if req, exists := reqs[requestID]; exists {
			return req, nil
		}
	}
	return nil, errors.New("pending request not found")
}

func (s *LocalConnectionStore) GetPendingConnectionByID(ctx context.Context, requestID string) (*PendingRequest, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	for _, reqs := range s.pendingConnections {
		if req, exists := reqs[requestID]; exists {
			return req, nil
		}
	}
	return nil, errors.New("pending request not found")
}

// --- Admin Connections ---
func (s *LocalConnectionStore) AddAdminConnection(ctx context.Context, roomName livekit.RoomName, adminID string, conn *websocket.Conn) {
	s.lock.Lock()
	defer s.lock.Unlock()
	
	if s.adminConnections[roomName] == nil {
		s.adminConnections[roomName] = make(map[string]*websocket.Conn)
	}
	s.adminConnections[roomName][adminID] = conn
	s.adminReverseLookup[conn] = adminID
}

func (s *LocalConnectionStore) GetAdminConnection(ctx context.Context, adminID string) *websocket.Conn {
	s.lock.RLock()
	defer s.lock.RUnlock()
	
	for _, roomConns := range s.adminConnections {
		if conn, exists := roomConns[adminID]; exists {
			return conn
		}
	}
	return nil
}

func (s *LocalConnectionStore) ListAdminConnections(ctx context.Context, roomName livekit.RoomName) []*websocket.Conn {
	s.lock.RLock()
	defer s.lock.RUnlock()
	
	var list []*websocket.Conn
	if roomConns := s.adminConnections[roomName]; roomConns != nil {
		for _, conn := range roomConns {
			list = append(list, conn)
		}
	}
	return list
}

func (s *LocalConnectionStore) RemoveAdminConnection(ctx context.Context, adminID string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	
	for roomName, roomConns := range s.adminConnections {
		if conn, exists := roomConns[adminID]; exists {
			delete(roomConns, adminID)
			delete(s.adminReverseLookup, conn)
			if len(roomConns) == 0 {
				delete(s.adminConnections, roomName)
			}
			break
		}
	}
}

func (s *LocalConnectionStore) CleanupRoom(ctx context.Context, roomName livekit.RoomName) bool {
	s.lock.Lock()
	defer s.lock.Unlock()

	roomExists := false
	if _, exists := s.activeConnections[roomName]; exists {
		roomExists = true
		delete(s.activeConnections, roomName)
	}
	
	if _, exists := s.pendingConnections[roomName]; exists {
		roomExists = true
		delete(s.pendingConnections, roomName)
	}
	
	if _, exists := s.adminConnections[roomName]; exists {
		roomExists = true
		delete(s.adminConnections, roomName)
	}
	
	return roomExists
}