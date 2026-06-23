// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Login handles user login.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	clientIP := getClientIP(r)
	if !h.loginRateLimiter.Allow(clientIP) {
		writeError(w, http.StatusTooManyRequests, "Too many login attempts. Try again later.")
		return
	}

	var creds models.UserLogin
	if err := readJSON(r, &creds); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	user, err := h.storage.GetUserByUsername(creds.Username)
	if err != nil || user == nil {
		// Generic error to prevent enumeration
		writeError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(creds.Password)); err != nil {
		writeError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}

	// Generate session token and update last login
	now := time.Now().UTC()
	token, err := generateToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate session token")
		return
	}
	tokenHash := models.HashToken(token)
	tokenExpiry := now.Add(models.SessionTokenDuration)
	user.LastLogin = &now
	user.SessionToken = &tokenHash
	user.TokenExpiresAt = &tokenExpiry

	// Store token and last_login in database
	if _, err := h.storage.AddUser(user); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update session")
		return
	}

	writeJSON(w, http.StatusOK, models.LoginResponse{
		Token: token,
		User: models.UserResponse{
			ID:        user.ID,
			Username:  user.Username,
			Role:      user.Role,
			Scopes:    user.Scopes,
			CreatedAt: user.CreatedAt,
		},
	})
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// CreateUser handles creating a new user (Admin only).
func (h *Handlers) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req models.UserCreate
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Validate username
	if len(req.Username) < 3 || len(req.Username) > 64 {
		writeError(w, http.StatusBadRequest, "Username must be between 3 and 64 characters")
		return
	}

	// Validate password
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "Password must be at least 8 characters")
		return
	}

	// Validate role
	validRoles := map[string]bool{"admin": true, "client": true, "readonly": true}
	if !validRoles[req.Role] {
		writeError(w, http.StatusBadRequest, "Invalid role. Must be one of: admin, client, readonly")
		return
	}

	// Check if user exists
	existing, _ := h.storage.GetUserByUsername(req.Username)
	if existing != nil {
		writeError(w, http.StatusConflict, "Username already exists")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to hash password")
		return
	}

	user := &models.User{
		ID:           uuid.New(),
		Username:     req.Username,
		PasswordHash: string(hash),
		Role:         req.Role,
		Scopes:       req.Scopes,
		CreatedAt:    time.Now().UTC(),
	}

	if _, err := h.storage.AddUser(user); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create user")
		return
	}

	writeJSON(w, http.StatusCreated, models.UserResponse{
		ID:        user.ID,
		Username:  user.Username,
		Role:      user.Role,
		Scopes:    user.Scopes,
		CreatedAt: user.CreatedAt,
	})
}
