// Copyright 2018 The Svalbard Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
///////////////////////////////////////////////////////////////////////////////

// Package svalbardsrv an implementation of a Svalbard HTTP server.
package svalbardsrv

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/google/svalbard/server/go/shareid"
)

// Canonical errors returned upon failures.
var (
	ErrExpectedPostRequest              = errors.New("expected POST request")
	ErrMissingToken                     = errors.New("missing token")
	ErrMissingShareValue                = errors.New("missing share_value")
	ErrMissingRequestID                 = errors.New("missing request_id")
	ErrShareAlreadyExists               = errors.New("share already exists")
	ErrShareNotFound                    = errors.New("share not found")
	ErrTokenNotFound                    = errors.New("token not found")
	ErrTokenExpired                     = errors.New("token expired")
	ErrTokenNotValid                    = errors.New("token not valid")
	ErrUnsupportedOwnerIDType           = errors.New("unsupported owner id type")
	ErrInvalidParametersForMsgWithToken = errors.New("invalid parameters for message with token")
	ErrInvalidMsgWithToken              = errors.New("invalid message with token")
	ErrInvalidShareID                   = errors.New("invalid share id")
	ErrInvalidShareValue                = errors.New("invalid share value")
)

// ShareStore enables storage and retrieval of shares identified by IDs.
// Every ShareStore implementation should in case of failures return
// only canonical errors defined above.
type ShareStore interface {
	// Store stores the given 'shareValue' under the specified 'shareID'.
	Store(shareID, shareValue string) error
	// Retrieve returns the value of the share identified by 'shareID',
	// if it is present in the store.  If no share is present, or if the
	// retrieval fails for some reason, it returns nil and an error message.
	Retrieve(shareID string) (string, error)
	// Delete removes from the store the share identified by 'shareID',
	// if it is present in the store.  If no share is present, or if the
	// deletion fails for some reason, it returns an error message.
	Delete(shareID string) error
}

// TokenStore generates short-lived "access" tokens for various operations,
// and checks their validity.
// Every TokenStore implementation should in case of failures return
// only canonical errors defined above.
type TokenStore interface {
	// GetNewToken returns a new access token valid for the operation 'op'
	// on the share identified by 'shareID'.
	GetNewToken(shareID string, op Operation) (string, error)
	// IsTokenValidNow returns nil if the given token is currently valid
	// for the operation 'op' on the share identified by 'shareID'.
	// Otherwise it returns an error indicating why the token is not valid.
	IsTokenValidNow(token, shareID string, op Operation) error
}

// TokenMsgData contains information needed to generate a message with a token
// to be sent using a secondary communication channel.
type TokenMsgData struct {
	ReqID string
	Token string
}

// RecipientID identifies an recipient and a communication channel.
type RecipientID struct {
	IDType string
	ID     string
}

// SecondaryChannel enables a secondary, one-way communication from server to client.
// It is used for sending short-lived tokens that authorize various operations.
// Every SecondaryChannel implementation must ensure that the errors returned
// upon failures contain no sensitive information.
type SecondaryChannel interface {
	// Send sends 'tokenMsgData' to the specified recipient.
	// If an error occurs, it returns a non-nil error value.
	Send(recipient RecipientID, tokenMsgData TokenMsgData) error
}

// GetMsgWithToken generates a message for the given 'data'.
func GetMsgWithToken(data TokenMsgData) (string, error) {
	if len(data.ReqID) < 1 || strings.Index(data.ReqID, ":") != -1 ||
		len(data.Token) < 1 || strings.Index(data.Token, ":") != -1 {
		return "", ErrInvalidParametersForMsgWithToken
	}
	return "SVBD:" + data.ReqID + ":" + data.Token, nil
}

// ParseMsgWithToken expects a message with token as generated by GetMsgWithToken,
// and parses it to provide the corresponding reqID and token separately.
func ParseMsgWithToken(msg string) (TokenMsgData, error) {
	if len(msg) < 5 || strings.ToUpper(msg[:5]) != "SVBD:" {
		return TokenMsgData{"", ""}, ErrInvalidMsgWithToken
	}
	parts := strings.Split(msg[5:], ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return TokenMsgData{"", ""}, ErrInvalidMsgWithToken
	}
	return TokenMsgData{ReqID: parts[0], Token: parts[1]}, nil
}

// Operation identifies operations guarded by the tokens.
type Operation int

// Operations that can be guarded by the tokens.
const (
	OpStoreShare Operation = iota
	OpRetrieveShare
	OpDeleteShare
)

// Server is a Svalbard server that stores shares and offers them for retrieval.
type Server struct {
	shareStore       ShareStore
	tokenStore       TokenStore
	secondaryChannel SecondaryChannel
}

// NewServer returns a new, initialized Svalbard server that uses the specified
// stores and offers all necessary handlers.
func NewServer(tokenStore TokenStore, shareStore ShareStore,
	secondaryChannel SecondaryChannel) *Server {
	return &Server{
		tokenStore:       tokenStore,
		shareStore:       shareStore,
		secondaryChannel: secondaryChannel,
	}
}

// GetStorageTokenHandler handles requests for a token that can be used to store a share.
// Request r must be a POST request with the following form data:
//  - request_id: an id of that particular request
//  - owner_id_type: type of id, e.g. "SMS", "email", ...
//  - owner_id:  actual id, e.g. a phone number, e-mail address
//  - secret_name: the name of the secret that the desired share belongs to
func (s *Server) GetStorageTokenHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("-------------- GET_STORAGE_TOKEN")
	log.Println(r)
	if r.Method != "POST" {
		http.Error(w, ErrExpectedPostRequest.Error(), http.StatusBadRequest)
		return
	}

	// Parse the request.
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("Parsing of POST data failed: %v\n", err)
		return
	}
	ownerIDType := r.FormValue("owner_id_type")
	ownerID := r.FormValue("owner_id")
	secretName := r.FormValue("secret_name")
	reqID := r.FormValue("request_id")
	log.Printf("Parsing of POST data succeeded: ownerIDType=[%v], ownerID=[%v], secretName=[%v], reqID=[%v]\n",
		ownerIDType, ownerID, secretName, reqID)

	// Verify the parsed parameters.
	if reqID == "" {
		http.Error(w, ErrMissingRequestID.Error(), http.StatusBadRequest)
		return
	}
	shareID, err := shareid.GetShareID(ownerIDType, ownerID, secretName)
	if err != nil {
		http.Error(w, errToPublicMessage(err), http.StatusBadRequest)
		return
	}
	// Check that the share does not exist yet.
	if _, err = s.shareStore.Retrieve(shareID); err == nil {
		http.Error(w, "Req. "+reqID+": share already exists.", http.StatusForbidden)
		return
	}

	// Generate a new storage token.
	token, err := s.tokenStore.GetNewToken(shareID, OpStoreShare)
	if err != nil {
		log.Printf("--- req. %s: generation of storage token for share of [%s] failed: %v\n",
			reqID, secretName, err)
		http.Error(w, "Req. "+reqID+": could not generate storage token, try later again.",
			http.StatusInternalServerError)
		return
	}

	// Send the token via the secondary channel.
	err = s.secondaryChannel.Send(RecipientID{ownerIDType, ownerID},
		TokenMsgData{reqID, token})
	if err != nil {
		http.Error(w, "Req. "+reqID+": error occurred while sending storage token: "+err.Error(),
			http.StatusInternalServerError)
	}

	// Log the operation, and prepare the response.
	log.Printf("--- req. %s: generated storage token [%s] for share of [%s] sent to [%s:%s]\n",
		reqID, token, secretName, ownerIDType, ownerID)
	fmt.Fprintf(w, "Req. %s: storage token for share of [%s] sent to [%s:%s]",
		reqID, secretName, ownerIDType, ownerID)
}

// StoreShareHandler handles requests that want to store a share.
// Request r must be a POST request with the following form data:
//  - token: the retrieval token that the client obtained via a secondary channel
//  - owner_id_type: type of id, e.g. "SMS", "email", ...
//  - owner_id:  actual id, e.g. a phone number, e-mail address
//  - secret_name: the name of the secret that the share_value belongs to
//  - share_value: the actual value of the share
func (s *Server) StoreShareHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("-------------- STORE_SHARE")
	log.Println(r)
	if r.Method != "POST" {
		http.Error(w, ErrExpectedPostRequest.Error(), http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("Parsing of POST data failed: %v\n", err)
		return
	}
	token := r.FormValue("token")
	if token == "" {
		http.Error(w, ErrMissingToken.Error(), http.StatusBadRequest)
		return
	}
	shareValue := r.FormValue("share_value")
	if shareValue == "" {
		http.Error(w, ErrMissingShareValue.Error(), http.StatusBadRequest)
		return
	}

	ownerIDType := r.FormValue("owner_id_type")
	ownerID := r.FormValue("owner_id")
	secretName := r.FormValue("secret_name")
	log.Printf("Parsing of POST data succeeded: ownerID=[%v], ownerIDType=[%v], secretName=[%v], token=[%v]\n",
		ownerIDType, ownerID, secretName, token)

	shareID, err := shareid.GetShareID(ownerIDType, ownerID, secretName)
	if err != nil {
		http.Error(w, errToPublicMessage(err), http.StatusBadRequest)
		return
	}
	err = s.tokenStore.IsTokenValidNow(token, shareID, OpStoreShare)
	if err != nil {
		http.Error(w, "could not store the share: "+errToPublicMessage(err), http.StatusForbidden)
		return
	}
	err = s.shareStore.Store(shareID, shareValue)
	if err != nil {
		if err == ErrShareAlreadyExists {
			http.Error(w, errToPublicMessage(err), http.StatusForbidden)
		} else {
			http.Error(w, errToPublicMessage(err), http.StatusInternalServerError)
		}
		return
	}
	log.Printf("--- stored a share of secret [%s] for owner [%s:%s]\n", secretName, ownerIDType, ownerID)
	fmt.Fprintf(w, "Stored a share of secret [%s] for owner [%s:%s]", secretName, ownerIDType, ownerID)
}

// GetRetrievalTokenHandler handles requests for a token that can be used to retrieve a share.
// Request r must be a POST request with the following form data:
//  - request_id: an id of that particular request
//  - owner_id_type: type of id, e.g. "SMS", "email", ...
//  - owner_id:  actual id, e.g. a phone number, e-mail address
//  - secret_name: the name of the secret that the desired share belongs to
func (s *Server) GetRetrievalTokenHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("-------------- GET_RETRIEVAL_TOKEN")
	log.Println(r)
	if r.Method != "POST" {
		http.Error(w, ErrExpectedPostRequest.Error(), http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("Parsing of POST data failed: %v\n", err)
		return
	}
	ownerIDType := r.FormValue("owner_id_type")
	ownerID := r.FormValue("owner_id")
	secretName := r.FormValue("secret_name")
	reqID := r.FormValue("request_id")
	log.Printf("Parsing of POST data succeeded: ownerID=[%v], ownerIDType=[%v], secretName=[%v], reqID=[%v]\n",
		ownerIDType, ownerID, secretName, reqID)

	if reqID == "" {
		http.Error(w, ErrMissingRequestID.Error(), http.StatusBadRequest)
		return
	}
	shareID, err := shareid.GetShareID(ownerIDType, ownerID, secretName)
	if err != nil {
		http.Error(w, errToPublicMessage(err), http.StatusBadRequest)
		return
	}
	_, err = s.shareStore.Retrieve(shareID)
	if err != nil {
		http.Error(w, "Req. "+reqID+": share not found.", http.StatusNotFound)
		return
	}
	token, err := s.tokenStore.GetNewToken(shareID, OpRetrieveShare)
	if err != nil {
		log.Printf("--- req. %s: generation of retrieval token for share of [%s] failed: %v\n",
			reqID, secretName, err)
		http.Error(w, "Req. "+reqID+": could not generate retrieval token, try later again.",
			http.StatusInternalServerError)
		return
	}
	err = s.secondaryChannel.Send(RecipientID{ownerIDType, ownerID},
		TokenMsgData{reqID, token})
	if err != nil {
		http.Error(w, "Req. "+reqID+": error occurred while sending retrieval token: "+err.Error(),
			http.StatusInternalServerError)
	}
	log.Printf("--- req. %s: generated retrieval token [%s] for share of [%s] sent to [%s:%s]\n",
		reqID, token, secretName, ownerIDType, ownerID)
	fmt.Fprintf(w, "Req. %s: retrieval token for share of [%s] sent to [%s:%s]",
		reqID, secretName, ownerIDType, ownerID)
}

// RetrieveShareHandler handles requests that want to retrieve a share.
// Request r must be a POST request with the following form data:
//  - token: the retrieval token that the client obtained via a secondary channel
//  - owner_id_type: type of id, e.g. "SMS", "email", ...
//  - owner_id:  actual id, e.g. a phone number, e-mail address
//  - secret_name: the name of the secret that the desired share belongs to
func (s *Server) RetrieveShareHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("-------------- RETRIEVE_SHARE")
	log.Println(r)
	if r.Method != "POST" {
		http.Error(w, ErrExpectedPostRequest.Error(), http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("Parsing of POST data failed: %v\n", err)
		return
	}
	token := r.FormValue("token")
	if token == "" {
		http.Error(w, ErrMissingToken.Error(), http.StatusBadRequest)
		return
	}

	ownerIDType := r.FormValue("owner_id_type")
	ownerID := r.FormValue("owner_id")
	secretName := r.FormValue("secret_name")
	log.Printf("Parsing of POST data succeeded: ownerID=[%v], ownerIDType=[%v], secretName=[%v], token=[%v]\n",
		ownerIDType, ownerID, secretName, token)

	shareID, err := shareid.GetShareID(ownerIDType, ownerID, secretName)
	if err != nil {
		http.Error(w, errToPublicMessage(err), http.StatusBadRequest)
		return
	}
	if err := s.tokenStore.IsTokenValidNow(token, shareID, OpRetrieveShare); err != nil {
		http.Error(w, "could not retrieve the share: "+errToPublicMessage(err), http.StatusForbidden)
		return
	}
	shareValue, err := s.shareStore.Retrieve(shareID)
	if err != nil {
		http.Error(w, "could not retrieve the share: "+errToPublicMessage(err), http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, shareValue)
}

// GetDeletionTokenHandler handles requests for a token that can be used to delete a share.
// Request r must be a POST request with the following form data:
//  - request_id: an id of that particular request
//  - owner_id_type: type of id, e.g. "SMS", "email", ...
//  - owner_id:  actual id, e.g. a phone number, e-mail address
//  - secret_name: the name of the secret that the desired share belongs to
func (s *Server) GetDeletionTokenHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("-------------- GET_DELETION_TOKEN")
	log.Println(r)
	if r.Method != "POST" {
		http.Error(w, ErrExpectedPostRequest.Error(), http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("Parsing of POST data failed: %v\n", err)
		return
	}
	ownerIDType := r.FormValue("owner_id_type")
	ownerID := r.FormValue("owner_id")
	secretName := r.FormValue("secret_name")
	reqID := r.FormValue("request_id")
	log.Printf("Parsing of POST data succeeded: ownerID=[%v], ownerIDType=[%v], secretName=[%v], reqID=[%v]\n",
		ownerIDType, ownerID, secretName, reqID)

	if reqID == "" {
		http.Error(w, ErrMissingRequestID.Error(), http.StatusBadRequest)
		return
	}
	shareID, err := shareid.GetShareID(ownerIDType, ownerID, secretName)
	if err != nil {
		http.Error(w, errToPublicMessage(err), http.StatusBadRequest)
		return
	}
	if _, err := s.shareStore.Retrieve(shareID); err != nil {
		http.Error(w, "Req. "+reqID+": share not found.", http.StatusNotFound)
		return
	}
	token, err := s.tokenStore.GetNewToken(shareID, OpDeleteShare)
	if err != nil {
		log.Printf("--- req. %s: generation of deletion token for share of [%s] failed: %v\n", reqID, secretName, err)
		http.Error(w, "Req. "+reqID+": could not generate deletion token, try later again.", http.StatusInternalServerError)
		return
	}
	if err := s.secondaryChannel.Send(RecipientID{ownerIDType, ownerID}, TokenMsgData{reqID, token}); err != nil {
		http.Error(w, "Req. "+reqID+": error occurred while sending deletion token: "+err.Error(),
			http.StatusInternalServerError)
	}
	log.Printf("--- req. %s: generated a deletion token [%s] for share of [%s] sent to [%s:%s]\n", reqID, token, secretName, ownerIDType, ownerID)
	fmt.Fprintf(w, "Req. %s: deletion token for share of [%s] sent to [%s:%s]", reqID, secretName, ownerIDType, ownerID)
}

// DeleteShareHandler handles requests that want to delete a share.
// Request r must be a POST request with the following form data:
//  - token: the deletion token that the client obtained via a secondary channel
//  - owner_id_type: type of id, e.g. "SMS", "email", ...
//  - owner_id:  actual id, e.g. a phone number, e-mail address
//  - secret_name: the name of the secret that the desired share belongs to
func (s *Server) DeleteShareHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("-------------- DELETE_SHARE")
	log.Println(r)
	if r.Method != "POST" {
		http.Error(w, ErrExpectedPostRequest.Error(), http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("Parsing of POST data failed: %v\n", err)
		return
	}
	token := r.FormValue("token")
	if token == "" {
		http.Error(w, ErrMissingToken.Error(), http.StatusBadRequest)
		return
	}

	ownerIDType := r.FormValue("owner_id_type")
	ownerID := r.FormValue("owner_id")
	secretName := r.FormValue("secret_name")
	log.Printf("Parsing of POST data succeeded: ownerID=[%v], ownerIDType=[%v], secretName=[%v], token=[%v]\n",
		ownerIDType, ownerID, secretName, token)

	shareID, err := shareid.GetShareID(ownerIDType, ownerID, secretName)
	if err != nil {
		http.Error(w, errToPublicMessage(err), http.StatusBadRequest)
		return
	}
	if err := s.tokenStore.IsTokenValidNow(token, shareID, OpDeleteShare); err != nil {
		http.Error(w, "could not delete the share: "+errToPublicMessage(err), http.StatusForbidden)
		return
	}
	if err := s.shareStore.Delete(shareID); err != nil {
		http.Error(w, "could not delete the share: "+errToPublicMessage(err), http.StatusInternalServerError)
		return
	}
	log.Printf("--- deleted a share of secret [%s] of owner [%s:%s]\n", secretName, ownerIDType, ownerID)
	fmt.Fprintf(w, "Deleted a share of secret [%s] of owner [%s:%s]", secretName, ownerIDType, ownerID)
}

// Known errors that are known not to contain any sensitive information.
var knownErrors = map[error]bool{
	shareid.ErrMissingOwnerType:         true,
	shareid.ErrMissingOwnerID:           true,
	shareid.ErrMissingSecretName:        true,
	ErrMissingToken:                     true,
	ErrMissingShareValue:                true,
	ErrMissingRequestID:                 true,
	ErrShareAlreadyExists:               true,
	ErrShareNotFound:                    true,
	ErrTokenNotFound:                    true,
	ErrTokenExpired:                     true,
	ErrTokenNotValid:                    true,
	ErrUnsupportedOwnerIDType:           true,
	ErrInvalidParametersForMsgWithToken: true,
	ErrInvalidMsgWithToken:              true,
}

// errToPublicMessage returns a message that describes the given error but is guaranteed
// to not contain any potentially sensitive information.
func errToPublicMessage(err error) string {
	if _, ok := knownErrors[err]; ok {
		return err.Error()
	}
	return "unknown error"
}
