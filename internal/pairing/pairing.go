package pairing

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
)

const pairingCodeTTL = 10 * time.Minute

type PendingRequest struct {
	Code      string
	UserID    string
	ChatID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

var (
	pendingRequests = make(map[string]*PendingRequest)
	mu              sync.Mutex
)

func generateCode() string {
	bytes := make([]byte, 3)
	rand.Read(bytes)
	return strings.ToUpper(hex.EncodeToString(bytes))
}

func CreatePairingRequest(userID, chatID string) string {
	mu.Lock()
	defer mu.Unlock()
	code := generateCode()
	now := time.Now()
	pendingRequests[userID] = &PendingRequest{
		Code:      code,
		UserID:    userID,
		ChatID:    chatID,
		CreatedAt: now,
		ExpiresAt: now.Add(pairingCodeTTL),
	}
	logger.Info(fmt.Sprintf("Pairing request created for user %s, code: %s", userID, code))
	return code
}

func ApprovePairingByCode(code string) bool {
	mu.Lock()
	defer mu.Unlock()
	pruneExpired()
	for userID, req := range pendingRequests {
		if req.Code == code {
			creds, err := config.LoadCredentials()
			if err != nil {
				return false
			}
			idSet := make(map[string]bool)
			for _, id := range creds.PairingAllow.IDs {
				idSet[id] = true
			}
			idSet[req.UserID] = true
			idSet[req.ChatID] = true
			newIDs := make([]string, 0, len(idSet))
			for id := range idSet {
				newIDs = append(newIDs, id)
			}
			creds.PairingAllow.IDs = newIDs
			if creds.PairingAllow.DefaultChatID == "" {
				creds.PairingAllow.DefaultChatID = req.ChatID
			}
			if err := config.SaveCredentials(creds); err != nil {
				return false
			}
			delete(pendingRequests, userID)
			logger.Info(fmt.Sprintf("Pairing approved for user %s, chatId: %s", userID, req.ChatID))
			return true
		}
	}
	return false
}

func IsAllowed(id string) bool {
	creds, err := config.LoadCredentials()
	if err != nil {
		return false
	}
	for _, allowedID := range creds.PairingAllow.IDs {
		if allowedID == id {
			return true
		}
	}
	return false
}

func GetDefaultChatID() string {
	creds, err := config.LoadCredentials()
	if err != nil {
		return ""
	}
	return creds.PairingAllow.DefaultChatID
}

func ListPending() []*PendingRequest {
	mu.Lock()
	defer mu.Unlock()
	pruneExpired()
	result := make([]*PendingRequest, 0, len(pendingRequests))
	for _, req := range pendingRequests {
		result = append(result, req)
	}
	return result
}

func pruneExpired() {
	now := time.Now()
	for key, req := range pendingRequests {
		if now.After(req.ExpiresAt) {
			delete(pendingRequests, key)
		}
	}
}
