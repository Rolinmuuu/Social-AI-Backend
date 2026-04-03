package utils

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

const (
	ErrCodeBadRequest          = 40000
	ErrCodeUnauthorized        = 40100
	ErrCodeUserAlreadyExisted  = 40001
	ErrCodeInvalidUser         = 40002
	ErrCodeForbidden           = 40300
	ErrCodeNotFound            = 40400
	ErrCodePostNotFound        = 40401
	ErrCodeCommentNotFound     = 40402
	ErrCodeLikeNotFound        = 40403
	ErrCodeShareNotFound       = 40404
	ErrCodeFollowNotFound      = 40405
	ErrCodeMessageNotFound     = 40406
	ErrCodeUserNotFound        = 40407
	ErrCodeAlreadyLiked        = 40408
	ErrCodeAlreadyFollowed     = 40409
	ErrCodeMethodNotAllowed    = 40500
	ErrCodeInternalServerError = 50000
	ErrCodeESFailed            = 50001
	ErrCodeRedisFailed         = 50002
	ErrCodeGCSFailed           = 50003
)

type APIResponse struct {
	RequestId string     `json:"request_id"`
	Error     *APIError  `json:"error,omitempty"`
	Data      interface{} `json:"data,omitempty"`
}

type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func WriteSuccess(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(APIResponse{
		RequestId: uuid.New().String(),
		Data:      data,
	})
}

func WriteError(w http.ResponseWriter, httpStatus, errCode int, errMessage string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(APIResponse{
		RequestId: uuid.New().String(),
		Error: &APIError{
			Code:    errCode,
			Message: errMessage,
		},
	})
}
