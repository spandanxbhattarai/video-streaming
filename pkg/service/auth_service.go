package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/guid"
)

type AuthService struct {
	upgrader  websocket.Upgrader
	config    *config.Config
	connStore ConnectionStore
	apiKey    string
	apiSecret string
}

type PendingRequest struct {
	ID           string             `json:"id"`
	RoomName     string             `json:"roomName"`
	UserInfo     AccessTokenOptions `json:"userInfo"`
	UserToken    string             `json:"userToken"`
	RequestTime  time.Time          `json:"requestTime"`
	Status       string             `json:"status"`
	AdminMessage string             `json:"adminMessage,omitempty"`
	UserConn     *websocket.Conn    `json:"-"`
	AdminConn    *websocket.Conn    `json:"-"`
}

type AccessTokenOptions struct {
	Identity   string            `json:"identity"`
	Name       string            `json:"name"`
	Metadata   string            `json:"metadata,omitempty"`
	Role       string            `json:"role"`
	RoomId     string            `json:"roomId"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type AuthEvent struct {
	Type      string      `json:"type"`
	Data      interface{} `json:"data"`
	RequestID string      `json:"requestId,omitempty"`
}

type JoinRequestEvent struct {
	RoomName  string             `json:"roomName"`
	UserInfo  AccessTokenOptions `json:"userInfo"`
	UserToken string             `json:"userToken,omitempty"`
}

type AdminNotificationEvent struct {
	RequestID   string             `json:"requestId"`
	RoomName    string             `json:"roomName"`
	UserInfo    AccessTokenOptions `json:"userInfo"`
	RequestTime time.Time          `json:"requestTime"`
}

type ApprovalResponseEvent struct {
	RequestIDs []string `json:"requestIds"` // Changed from single RequestID to array
	Approved   bool     `json:"approved"`
	Message    string   `json:"message,omitempty"`
}

type TokenResponseEvent struct {
	Token     string `json:"token"`
	RequestID string `json:"requestId"`
	Message   string `json:"message,omitempty"`
	IsAdmin   bool   `json:"isAdmin"`
}

type ErrorEvent struct {
	Error     string `json:"error"`
	RequestID string `json:"requestId,omitempty"`
}

type BulkApprovalNotificationEvent struct {
	RequestIDs   []string           `json:"requestIds"`
	RoomName     string             `json:"roomName"`
	AcceptedUsers []AccessTokenOptions `json:"acceptedUsers"`
	ApprovalTime time.Time          `json:"approvalTime"`
	Message      string             `json:"message,omitempty"`
}

type CleanupRequest struct {
	RoomName string `json:"roomName"`
}

type CleanupResponse struct {
	Success      bool     `json:"success"`
	Message      string   `json:"message"`
	RemovedRooms []string `json:"removedRooms,omitempty"`
}

func NewAuthService(conf *config.Config, apiKey, apiSecret string, connStore ConnectionStore) *AuthService {
	return &AuthService{
		upgrader: websocket.Upgrader{
			EnableCompression: true,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		config:    conf,
		connStore: connStore,
		apiKey:    apiKey,
		apiSecret: apiSecret,
	}
}

func (s *AuthService) SetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/auth", s.handleAuth)
	mux.HandleFunc("/auth/cleanup", s.handleCleanup)
}

func (s *AuthService) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req CleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON format", http.StatusBadRequest)
		return
	}

	if req.RoomName == "" {
		http.Error(w, "Room name is required", http.StatusBadRequest)
		return
	}

	ctx := context.Background()
	roomExists := s.connStore.CleanupRoom(ctx, livekit.RoomName(req.RoomName))

	var response CleanupResponse
	if roomExists {
		logger.Infow("room cleanup completed", "roomName", req.RoomName)
		response = CleanupResponse{
			Success:      true,
			Message:      fmt.Sprintf("Room %s cleaned up successfully", req.RoomName),
			RemovedRooms: []string{req.RoomName},
		}
	} else {
		logger.Infow("room not found for cleanup", "roomName", req.RoomName)
		response = CleanupResponse{
			Success:      false,
			Message:      fmt.Sprintf("Room %s not found or already cleaned up", req.RoomName),
			RemovedRooms: []string{},
		}
		w.WriteHeader(http.StatusNotFound)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *AuthService) handleAuth(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "WebSocket upgrade required", http.StatusBadRequest)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Errorw("failed to upgrade auth connection", err)
		return
	}
	defer func() {
		s.handleConnectionDisconnect(conn)
		conn.Close()
	}()

	logger.Infow("auth connection established (unified endpoint)")

	for {
		var event AuthEvent
		if err := conn.ReadJSON(&event); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				logger.Errorw("auth connection closed unexpectedly", err)
			}
			break
		}

		switch event.Type {
		case "join_request":
			s.handleJoinRequest(conn, event)
		case "join_response":
			s.handleJoinResponse(conn, event)
		default:
			s.sendError(conn, "unknown event type", "")
		}
	}
}

func (s *AuthService) handleJoinRequest(conn *websocket.Conn, event AuthEvent) {
    var joinReq JoinRequestEvent
    dataBytes, _ := json.Marshal(event.Data)
    if err := json.Unmarshal(dataBytes, &joinReq); err != nil {
        s.sendError(conn, "invalid join request format", "")
        return
    }

    resolvedInfo, ok := s.validateUser(joinReq.UserToken, joinReq.UserInfo)
    if !ok {
        s.sendError(conn, "user validation failed", "")
        return
    }
    joinReq.UserInfo = resolvedInfo
    isHost, err := s.isHost(joinReq.UserInfo.RoomId, joinReq.UserToken)
    if err != nil {
        // Meeting is not valid - send error instead of treating as normal user
        s.sendError(conn, fmt.Sprintf("Meeting validation failed: %s", err.Error()), "")
        return
    }

    if isHost {
        adminID := guid.New("ADMIN_")
        ctx := context.Background()
        s.connStore.AddAdminConnection(ctx, livekit.RoomName(joinReq.RoomName), adminID, conn)
        
        logger.Infow("admin connection registered", 
            "roomName", joinReq.RoomName,
            "adminID", adminID)
        
        s.replayPendingForAdmin(conn, joinReq.RoomName)

        token := s.createParticipantToken(joinReq.UserInfo, joinReq.RoomName, isHost, "")
        s.sendTokenResponse(conn, token, "", "Host access granted", true)
        return
    }
	requestID := guid.New("REQ_")
	pendingReq := &PendingRequest{
		ID:          requestID,
		RoomName:    joinReq.RoomName,
		UserInfo:    joinReq.UserInfo,
		UserToken:   joinReq.UserToken,
		RequestTime: time.Now(),
		Status:      "pending",
		UserConn:    conn,
	}

	// Add pending request to store
	ctx := context.Background()
	s.connStore.AddPendingConnection(ctx, livekit.RoomName(joinReq.RoomName), pendingReq)

	s.notifyAdmin(pendingReq)
	s.sendPendingResponse(conn, requestID)
}

func (s *AuthService) handleJoinResponse(conn *websocket.Conn, event AuthEvent) {
    logger.Infow("handleJoinResponse called", 
        "eventType", event.Type, 
        "rawData", fmt.Sprintf("%+v", event.Data))

    var approval ApprovalResponseEvent
    dataBytes, err := json.Marshal(event.Data)
    if err != nil {
        logger.Infow("failed to marshal event data", "error", err)
        s.sendError(conn, "invalid event data format", "")
        return
    }
    
    logger.Infow("raw approval data", "json", string(dataBytes))
    
    if err := json.Unmarshal(dataBytes, &approval); err != nil {
        logger.Infow("failed to unmarshal approval response", 
            "error", err, 
            "data", string(dataBytes))
        s.sendError(conn, "invalid approval response format", "")
        return
    }
    
    logger.Infow("approval response parsed", 
        "requestIDs", approval.RequestIDs,
        "approved", approval.Approved,
        "message", approval.Message)

    if len(approval.RequestIDs) == 0 {
        s.sendError(conn, "no request IDs provided", "")
        return
    }

    // Get pending requests from store
    ctx := context.Background()
    
    // Get pending requests and determine room name from the requests
    var pendingReqs []*PendingRequest
    var roomName livekit.RoomName
    
    // Get all pending requests for the given request IDs
    for _, requestID := range approval.RequestIDs {
        pendingReq, err := s.connStore.GetPendingConnectionByID(ctx, requestID)
        if err != nil {
            logger.Infow("pending request not found", 
                "requestID", requestID,
                "error", err)
            continue
        }
        pendingReqs = append(pendingReqs, pendingReq)

        if roomName == "" {
            roomName = livekit.RoomName(pendingReq.RoomName)
        }
    }
    
    logger.Infow("processing approval for request IDs", 
        "requestIDs", approval.RequestIDs,
        "room", roomName,
        "foundRequests", len(pendingReqs))
    logger.Infow("pending requests found", "count", len(pendingReqs))
    
    // Create a map for quick lookup
    pendingReqMap := make(map[string]*PendingRequest)
    for _, req := range pendingReqs {
        pendingReqMap[req.ID] = req
    }

    var processedRequests []*PendingRequest
    var acceptedUsers []AccessTokenOptions

    // Process each request ID
    for _, requestID := range approval.RequestIDs {
        pendingReq, exists := pendingReqMap[requestID]
        if !exists {
            logger.Infow("pending request not found", 
                "requestID", requestID,
                "availableRequests", len(pendingReqs))
            continue
        }
        
        logger.Infow("processing pending request", 
            "requestID", pendingReq.ID,
            "status", pendingReq.Status,
            "user", pendingReq.UserInfo.Identity,
            "room", pendingReq.RoomName)

        if approval.Approved {
            pendingReq.Status = "approved"
            logger.Infow("request approved by admin", 
                "requestID", requestID,
                "user", pendingReq.UserInfo.Identity)
            acceptedUsers = append(acceptedUsers, pendingReq.UserInfo)
        } else {
            pendingReq.Status = "rejected"
            logger.Infow("request rejected by admin", 
                "requestID", requestID,
                "user", pendingReq.UserInfo.Identity,
                "reason", approval.Message)
        }
        pendingReq.AdminMessage = approval.Message
        processedRequests = append(processedRequests, pendingReq)

        // Send response to user
        if pendingReq.UserConn != nil {
            if approval.Approved {
                logger.Infow("creating participant token", 
                    "user", pendingReq.UserInfo.Identity,
                    "room", pendingReq.RoomName)
                    
                token := s.createParticipantToken(pendingReq.UserInfo, pendingReq.RoomName, false, pendingReq.UserToken)
                
                logger.Infow("sending token response to user",
                    "requestID", requestID,
                    "user", pendingReq.UserInfo.Identity)
                s.sendTokenResponse(pendingReq.UserConn, token, requestID, approval.Message, false)
            } else {
                logger.Infow("sending rejection response to user",
                    "requestID", requestID,
                    "user", pendingReq.UserInfo.Identity)
                    
                s.sendRejectionResponse(pendingReq.UserConn, requestID, approval.Message)
            }
        } else {
            logger.Infow("user connection is nil - cannot send response",
                "requestID", requestID,
                "user", pendingReq.UserInfo.Identity)
        }

        // Remove pending request from store
        logger.Infow("removing pending connection",
            "room", pendingReq.RoomName,
            "requestID", requestID)
            
        s.connStore.RemovePendingConnection(ctx, livekit.RoomName(pendingReq.RoomName), requestID)
    }

    // Notify admin about bulk approval if users were accepted
    if approval.Approved && len(acceptedUsers) > 0 {
        s.notifyAdminBulkApproval(approval.RequestIDs, roomName, acceptedUsers, approval.Message)
    }
    
    logger.Infow("bulk join request processing completed",
        "requestIDs", approval.RequestIDs,
        "processedCount", len(processedRequests),
        "acceptedCount", len(acceptedUsers))
}

func (s *AuthService) createParticipantToken(userInfo AccessTokenOptions, roomName string, isHost bool, userToken string) string {
	logger.Infow("createParticipantToken called",
		"identity", userInfo.Identity,
		"name", userInfo.Name,
		"metadata", userInfo.Metadata,
		"room", roomName,
		"isHost", isHost,
	)

	// Initialize metadata
	metadata := make(map[string]interface{})
	if userInfo.Metadata != "" {
		json.Unmarshal([]byte(userInfo.Metadata), &metadata)
	}
	metadata["status"] = "active"

	// If not host, make API call to get meeting data
	if !isHost && userToken != "" {
		candidateID, err := s.getMeetingIDFromAPI(roomName, userToken)
		if err != nil {
			logger.Errorw("failed to get meeting ID from API", err,
				"roomName", roomName)
		} else if candidateID != 0 {
			metadata["candidate_id"] = candidateID
			logger.Infow("added meeting ID to token metadata",
				"meetingID", candidateID,
				"roomName", roomName)
		}
	}

	metadataBytes, _ := json.Marshal(metadata)

	at := auth.NewAccessToken(s.apiKey, s.apiSecret).
		SetIdentity(userInfo.Identity).
		SetValidFor(5 * time.Minute).
		SetMetadata(string(metadataBytes))
	
	if userInfo.Name != "" {
		at.SetName(userInfo.Name)
	}
	
	grant := &auth.VideoGrant{
		Room:     roomName,
		RoomJoin: true,
	}
	grant.SetCanPublish(true)
	grant.SetCanPublishData(true)
	grant.SetCanUpdateOwnMetadata(true)
	grant.SetCanSubscribe(true)
	at.AddGrant(grant)

	token, err := at.ToJWT()
	if err != nil {
		logger.Errorw("failed to create JWT token", err)
		return ""
	}
	return token 
}

func (s *AuthService) getMeetingIDFromAPI(roomName, userToken string) (int, error) {
	payload := map[string]string{
		"meet_id": roomName,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		logger.Errorw("failed to marshal JSON payload", err, "roomName", roomName)
		return 0, fmt.Errorf("failed to marshal JSON payload: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api-staging.jobbicus.com/api/v1/enter-room", bytes.NewBuffer(jsonData))
	if err != nil {
		logger.Errorw("failed to create HTTP request", err, "roomName", roomName)
		return 0, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logger.Errorw("failed to make API call", err, "roomName", roomName)
		return 0, fmt.Errorf("failed to make API call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.Errorw("API returned non-200 status", nil, 
			"statusCode", resp.StatusCode, 
			"roomName", roomName)
		return 0, fmt.Errorf("API returned status: %d", resp.StatusCode)
	}

	var response struct  {
		Success bool `json:"success"`
		IsValid bool `json:"is_valid"`
		IsHost  bool `json:"is_host"`
	
		MeetData struct {
			ID          int    `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Date        string `json:"date"`
			StartTime   string `json:"start_time"`
			Duration    string `json:"duration"`
			EndTime     string `json:"end_time"`
			MeetLink    string `json:"meet_link"`
			RoomID      string `json:"room_id"`
			TimeZone    string `json:"time_zone"`
			ExpiresAt   string `json:"expires_at"`
		} `json:"meet_data"`
	
		UserData struct {
			ID                   int    `json:"id"`
			Username             string `json:"username"`
			Email                string `json:"email"`
			EmailVerifiedAt      string `json:"email_verified_at"`
			UserType             string `json:"user_type"`
			RememberToken        string `json:"remember_token"`
			CreatedAt            string `json:"created_at"`
			UpdatedAt            string `json:"updated_at"`
			FirebaseToken        string `json:"firebase_token"`
			FirebaseTokenTime    string `json:"firebase_token_timestamp"`
			Remember             bool   `json:"remember"`
			LastLogin            string `json:"last_login"`
			Candidate struct {
				ID int `json:"id"`
			} `json:"candidate"`
		} `json:"user_data"`
	}
	

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		logger.Errorw("failed to decode API response", err, "roomName", roomName)
		return 0, fmt.Errorf("failed to decode API response: %w", err)
	}

	if !response.Success {
		logger.Infow("API call unsuccessful", 
			"success", response.Success, 
			"roomName", roomName)
		return 0, fmt.Errorf("API call unsuccessful for room %s", roomName)
	}

	if !response.IsValid {
		logger.Errorw("Invalid meeting", fmt.Errorf("meeting is invalid"), 
			"roomName", roomName, 
			"meetingTitle", response.MeetData.Title)
		return 0, fmt.Errorf("meeting is not valid for room %s", roomName)
	}

	logger.Infow("successfully retrieved meeting data", 
		"roomName", roomName, 
		"meetingID", response.UserData.Candidate.ID,
		"meetingTitle", response.MeetData.Title)

	return response.UserData.Candidate.ID, nil
}

func (s *AuthService) notifyAdmin(pendingReq *PendingRequest) {
	ctx := context.Background()
	adminConns := s.connStore.ListAdminConnections(ctx, livekit.RoomName(pendingReq.RoomName))
	
	logger.Infow("notifyAdmin called", 
		"roomName", pendingReq.RoomName, 
		"adminCount", len(adminConns))
	
	if len(adminConns) == 0 {
		logger.Infow("no admin registered for room", nil, "roomName", pendingReq.RoomName)
		return
	}
	
	notification := AuthEvent{
		Type: "join_request_notification",
		Data: AdminNotificationEvent{
			RequestID:   pendingReq.ID,
			RoomName:    pendingReq.RoomName,
			UserInfo:    pendingReq.UserInfo,
			RequestTime: pendingReq.RequestTime,
		},
	}
	
	for _, adminConn := range adminConns {
		if adminConn != nil {
			if err := adminConn.WriteJSON(notification); err != nil {
				logger.Errorw("failed to send notification to admin", err, 
					"roomName", pendingReq.RoomName)
			} else {
				logger.Infow("notification sent to admin", 
					"roomName", pendingReq.RoomName, 
					"requestID", pendingReq.ID)
			}
		}
	}
}
func (s *AuthService) sendTokenResponse(conn *websocket.Conn, token, requestID, message string, admin bool) {
	response := AuthEvent{
		Type: "token_response",
		Data: TokenResponseEvent{
			Token:     token,
			RequestID: requestID,
			Message:   message,
			IsAdmin:   admin,
		},
	}
	conn.WriteJSON(response)
}

func (s *AuthService) sendPendingResponse(conn *websocket.Conn, requestID string) {
	response := AuthEvent{
		Type: "pending_response",
		Data: map[string]interface{}{
			"requestId": requestID,
			"message":   "Request sent to admin for approval",
		},
	}
	conn.WriteJSON(response)
}

func (s *AuthService) sendRejectionResponse(conn *websocket.Conn, requestID, message string) {
	response := AuthEvent{
		Type: "rejection_response",
		Data: map[string]interface{}{
			"requestId": requestID,
			"message":   message,
		},
	}
	conn.WriteJSON(response)
}

func (s *AuthService) sendError(conn *websocket.Conn, errorMsg, requestID string) {
	response := AuthEvent{
		Type: "error",
		Data: ErrorEvent{
			Error:     errorMsg,
			RequestID: requestID,
		},
	}
	conn.WriteJSON(response)
}
func (s *AuthService) handleConnectionDisconnect(conn *websocket.Conn) {
	ctx := context.Background()
	logger.Infow("connection disconnected, cleaning up")

	pendingReqs := s.connStore.ListAllPendingConnections(ctx)
	for _, req := range pendingReqs {
		if req.UserConn == conn {
			logger.Infow("pending user connection disconnected", 
				"requestID", req.ID, 
				"user", req.UserInfo.Identity,
				"room", req.RoomName)
			
			s.connStore.RemovePendingConnection(ctx, livekit.RoomName(req.RoomName), req.ID)
			s.notifyAdminUserDisconnected(req)
		}
	}
	

	// Remove from admin connections and notify users
	removed, roomName, adminID := s.connStore.RemoveAdminConnectionByConn(ctx, conn)
	if removed {
		logger.Infow("admin connection disconnected", 
			"remoteAddr", conn.RemoteAddr().String(),
			"adminID", adminID,
			"room", roomName)
		
		// Notify pending users about admin disconnect
		s.notifyUsersAdminDisconnected(roomName, adminID)
	}
	
	logger.Infow("connection cleanup completed", "remoteAddr", conn.RemoteAddr().String())
}

func (s *AuthService) notifyUsersAdminDisconnected(roomName livekit.RoomName, adminID string) {
	ctx := context.Background()
	pendingReqs := s.connStore.ListPendingConnections(ctx, roomName)
	
	if len(pendingReqs) == 0 {
		return
	}

	// Notify pending users that admin is no longer available
	for _, req := range pendingReqs {
		if req.UserConn != nil {
			notification := AuthEvent{
				Type: "admin_disconnected_notification",
				Data: map[string]interface{}{
					"message":   "Admin has disconnected. Your request may be delayed.",
					"requestId": req.ID,
				},
			}
			
			if err := req.UserConn.WriteJSON(notification); err != nil {
				logger.Errorw("failed to notify user about admin disconnect", err,
					"requestID", req.ID, "roomName", roomName)
			}
		}
	}
}

func (s *AuthService) notifyAdminBulkApproval(requestIDs []string, roomName livekit.RoomName, acceptedUsers []AccessTokenOptions, message string) {
	ctx := context.Background()
	adminConns := s.connStore.ListAdminConnections(ctx, roomName)
	
	logger.Infow("notifyAdminBulkApproval called", 
		"roomName", roomName, 
		"adminCount", len(adminConns),
		"acceptedUsersCount", len(acceptedUsers))
	
	if len(adminConns) == 0 {
		logger.Infow("no admin registered for room", nil, "roomName", roomName)
		return
	}
	
	notification := AuthEvent{
		Type: "bulk_approval_notification",
		Data: BulkApprovalNotificationEvent{
			RequestIDs:    requestIDs,
			RoomName:      string(roomName),
			AcceptedUsers: acceptedUsers,
			ApprovalTime:  time.Now(),
			Message:       message,
		},
	}
	
	for _, adminConn := range adminConns {
		if adminConn != nil {
			if err := adminConn.WriteJSON(notification); err != nil {
				logger.Errorw("failed to send bulk approval notification to admin", err, 
					"roomName", roomName)
			} else {
				logger.Infow("bulk approval notification sent to admin", 
					"roomName", roomName, 
					"requestIDs", requestIDs,
					"acceptedUsersCount", len(acceptedUsers))
			}
		}
	}
}

func (s *AuthService) notifyAdminUserDisconnected(req *PendingRequest) {
	ctx := context.Background()
	logger.Infow("Notifying admin")
	adminConns := s.connStore.ListAdminConnections(ctx, livekit.RoomName(req.RoomName))
	
	if len(adminConns) == 0 {
		logger.Infow("no admin registered for room on user disconnect", nil, "roomName", req.RoomName)
		return
	}

	notification := AuthEvent{
		Type: "user_disconnected_notification",
		Data: map[string]interface{}{
			"requestId":   req.ID,
			"roomName":    req.RoomName,
			"userInfo":    req.UserInfo,
			"requestTime": req.RequestTime,
			"status":      "disconnected",
			"reason":      "connection closed",
		},
	}

	for _, adminConn := range adminConns {
		if adminConn != nil {
			if err := adminConn.WriteJSON(notification); err != nil {
				logger.Errorw("failed to send user disconnect notification to admin", err, 
					"roomName", req.RoomName, "requestID", req.ID)
			} else {
				logger.Infow("user disconnect notification sent to admin", 
					"roomName", req.RoomName, 
					"requestID", req.ID)
			}
		}
	}
}
func (s *AuthService) GetPendingRequests(roomName string) []*PendingRequest {
    ctx := context.Background()
	return s.connStore.ListPendingConnections(ctx, livekit.RoomName(roomName))
}

func (s *AuthService) isHost(roomId string, userToken string) (bool, error) {
    payload := map[string]string{
        "meet_id": roomId,
    }

    jsonData, err := json.Marshal(payload)
    if err != nil {
        logger.Errorw("failed to marshal JSON payload", err, "roomId", roomId)
        return false, fmt.Errorf("failed to marshal JSON payload: %w", err)
    }

    req, err := http.NewRequest("POST", "https://api-staging.jobbicus.com/api/v1/enter-room", bytes.NewBuffer(jsonData))
    if err != nil {
        logger.Errorw("failed to create HTTP request", err, "roomId", roomId)
        return false, fmt.Errorf("failed to create HTTP request: %w", err)
    }

    req.Header.Set("Accept", "application/json")
    req.Header.Set("Authorization", "Bearer "+userToken)
    req.Header.Set("Content-Type", "application/json")

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        logger.Errorw("failed to make API call", err, "roomId", roomId)
        return false, fmt.Errorf("failed to make API call: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        logger.Errorw("API returned non-200 status", nil, 
            "statusCode", fmt.Sprintf("%d", resp.StatusCode), 
            "roomId", roomId)
        return false, fmt.Errorf("API returned non-200 status: %d", resp.StatusCode)
    }

    var response struct {
        Success  bool `json:"success"`
        IsValid  bool `json:"is_valid"`
        IsHost   bool `json:"is_host"`
        MeetData struct {
            ID          int    `json:"id"`
            Title       string `json:"title"`
            Description string `json:"description"`
            Date        string `json:"date"`
            StartTime   string `json:"start_time"`
            Duration    string `json:"duration"`
            EndTime     string `json:"end_time"`
            MeetLink    string `json:"meet_link"`
            RoomID      string `json:"room_id"`
            TimeZone    string `json:"time_zone"`
            ExpiresAt   string `json:"expires_at"`
        } `json:"meet_data"`
    }

    if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
        logger.Errorw("failed to decode API response", err, "roomId", roomId)
        return false, fmt.Errorf("failed to decode API response: %w", err)
    }

    if !response.Success {
        logger.Infow("API call unsuccessful", 
            "success", fmt.Sprintf("%v", response.Success), 
            "roomId", roomId)
        return false, fmt.Errorf("API call unsuccessful for room %s", roomId)
    }

    if !response.IsValid {
        // FIXED: Proper error logging
        logger.Errorw("Invalid meeting", fmt.Errorf("meeting is invalid"), 
            "roomId", roomId, 
            "meetingTitle", response.MeetData.Title)
        return false, fmt.Errorf("meeting is not valid for room %s", roomId)
    }

    if response.IsHost {
        logger.Infow("host detected", "roomId", roomId, "meetingTitle", response.MeetData.Title)
        return true, nil
    }

    logger.Infow("user is not a host", "roomId", roomId, "meetingTitle", response.MeetData.Title)
    return false, nil
}

func (s *AuthService) validateUser(userToken string, fallback AccessTokenOptions) (AccessTokenOptions, bool) {
	resolved := fallback
	resolved.Role = "host"
	return resolved, true
}

func (s *AuthService) GetAdminConnectionInfo() map[string]int {
	result := make(map[string]int)

	if localStore, ok := s.connStore.(*LocalConnectionStore); ok {
		localStore.lock.RLock()
		defer localStore.lock.RUnlock()
		
		for roomName, roomConns := range localStore.adminConnections {
			result[string(roomName)] = len(roomConns)
		}
	}
	
	return result
}

func (s *AuthService) replayPendingForAdmin(adminConn *websocket.Conn, roomName string) {
	ctx := context.Background()
	pendingReqs := s.connStore.ListPendingConnections(ctx, livekit.RoomName(roomName))
	for _, req := range pendingReqs {
		if req.Status != "pending" {
			continue
		}
		notification := AuthEvent{
			Type: "join_request_notification",
			Data: AdminNotificationEvent{
				RequestID:   req.ID,
				RoomName:    req.RoomName,
				UserInfo:    req.UserInfo,
				RequestTime: req.RequestTime,
			},
		}
		if err := adminConn.WriteJSON(notification); err != nil {
			logger.Errorw("failed to replay pending request to admin", err, 
				"requestID", req.ID)
		}
	}
}