package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/nitecon/amux/storage"
)

func mintBrowserSessionToken(apiKey string, user storage.User, expiresMS int64) string {
	return strconv.FormatInt(user.ID, 10) + "." + strconv.FormatInt(expiresMS, 10) + "." +
		browserSessionSignature(apiKey, user.ID, expiresMS, user.SessionEpoch)
}

func browserSessionTokenUserID(token string) (int64, bool) {
	userIDText, _, _ := strings.Cut(token, ".")
	if userIDText == "" {
		return 0, false
	}
	userID, err := strconv.ParseInt(userIDText, 10, 64)
	return userID, err == nil
}

func verifyBrowserSessionToken(apiKey string, user storage.User, token string, nowMS int64) bool {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return false
	}
	tokenUserID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	expiresMS, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || tokenUserID != user.ID || expiresMS <= nowMS || !user.IsActive() {
		return false
	}
	expected := browserSessionSignature(apiKey, user.ID, expiresMS, user.SessionEpoch)
	return hmac.Equal([]byte(expected), []byte(parts[2]))
}

func browserSessionSignature(apiKey string, userID, expiresMS, sessionEpoch int64) string {
	mac := hmac.New(sha256.New, []byte(apiKey))
	_, _ = mac.Write([]byte("gateway-ui-session:" + strconv.FormatInt(userID, 10) + ":" +
		strconv.FormatInt(expiresMS, 10) + ":" + strconv.FormatInt(sessionEpoch, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}
