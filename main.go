package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type Item struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type tokenBucket struct {
	tokens     float64
	max        float64
	refillRate float64
	lastRefill time.Time
	mu         sync.Mutex
}

func (tb *tokenBucket) allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens += elapsed * tb.refillRate
	if tb.tokens > tb.max {
		tb.tokens = tb.max
	}
	tb.lastRefill = now
	if tb.tokens < 1 {
		return false
	}
	tb.tokens--
	return true
}

type rateLimiter struct {
	limit    float64
	bucket   *tokenBucket
	mu       sync.Mutex
	clientDB map[string]*tokenBucket
}

func newRateLimiter(limit float64) *rateLimiter {
	return &rateLimiter{
		limit:    limit,
		bucket:   &tokenBucket{max: limit, refillRate: limit, lastRefill: time.Now()},
		clientDB: make(map[string]*tokenBucket),
	}
}

func (rl *rateLimiter) allow(clientAddr string) bool {
	if rl.limit <= 0 {
		return true
	}
	rl.mu.Lock()
	tb, exists := rl.clientDB[clientAddr]
	if !exists || time.Since(tb.lastRefill) > 60*time.Second {
		tb = &tokenBucket{max: rl.limit, refillRate: rl.limit, lastRefill: time.Now()}
		rl.clientDB[clientAddr] = tb
	}
	rl.mu.Unlock()
	return tb.allow()
}

func (rl *rateLimiter) cleanup() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		for addr, tb := range rl.clientDB {
			if time.Since(tb.lastRefill) > 5*time.Minute {
				delete(rl.clientDB, addr)
			}
		}
		rl.mu.Unlock()
	}
}

func main() {
	table := os.Getenv("DYNAMODB_TABLE")
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	rateLimitStr := os.Getenv("RATE_LIMIT")
	rateLimit := 10.0
	if rateLimitStr != "" {
		if n, err := strconv.ParseFloat(rateLimitStr, 64); err == nil && n >= 0 {
			rateLimit = n
		}
	}

	limiter := newRateLimiter(rateLimit)
	go limiter.cleanup()

	if table == "" || region == "" {
		log.Fatal("DYNAMODB_TABLE and AWS_REGION required")
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		log.Fatalf("failed to load aws config: %v", err)
	}
	db := dynamodb.NewFromConfig(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/", homeHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/api/items", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/items")
		path = strings.TrimSuffix(path, "/")

		switch {
		case path == "" && r.Method == http.MethodGet:
			listItems(w, r, db, table)
		case path == "" && r.Method == http.MethodPost:
			createItem(w, r, db, table)
		case path != "" && r.Method == http.MethodGet:
			getItem(w, r, path, db, table)
		case path != "" && r.Method == http.MethodDelete:
			deleteItem(w, r, path, db, table)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	srv := &http.Server{Addr: ":" + port, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientAddr := r.RemoteAddr
		if !limiter.allow(clientAddr) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		mux.ServeHTTP(w, r)
	})}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	log.Printf("starting on :%s", port)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"service": "go-http-api"})
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func createItem(w http.ResponseWriter, r *http.Request, db *dynamodb.Client, table string) {
	defer io.Copy(io.Discard, r.Body)
	defer r.Body.Close()

	var req struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	item := Item{ID: generateID(), Name: req.Name, Description: req.Description}
	attr := map[string]types.AttributeValue{
		"id":   &types.AttributeValueMemberS{Value: item.ID},
		"name": &types.AttributeValueMemberS{Value: item.Name},
	}
	if item.Description != "" {
		attr["description"] = &types.AttributeValueMemberS{Value: item.Description}
	}
	db.PutItem(r.Context(), &dynamodb.PutItemInput{TableName: &table, Item: attr})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(item)
}

func listItems(w http.ResponseWriter, r *http.Request, db *dynamodb.Client, table string) {
	limit := 20
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, _ := strconv.Atoi(q); n > 0 && n <= 100 {
			limit = n
		}
	}

	input := &dynamodb.QueryInput{
		TableName:              &table,
		Limit:                  awsInt32(int32(limit)),
		KeyConditionExpression: awsStr("begins_with(id, :id)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":id": &types.AttributeValueMemberS{Value: ""},
		},
	}
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		var keys map[string]types.AttributeValue
		if err := json.Unmarshal([]byte(cursor), &keys); err == nil {
			input.ExclusiveStartKey = keys
		}
	}

	result, err := db.Query(r.Context(), input)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	var items []Item
	for _, attr := range result.Items {
		var item Item
		for k, v := range attr {
			if av, ok := v.(*types.AttributeValueMemberS); ok {
				switch k {
				case "id":
					item.ID = av.Value
				case "name":
					item.Name = av.Value
				case "description":
					item.Description = av.Value
				}
			}
		}
		items = append(items, item)
	}

	nextKey, _ := json.Marshal(result.LastEvaluatedKey)
	json.NewEncoder(w).Encode(map[string]any{"items": items, "nextKey": string(nextKey)})
}

func getItem(w http.ResponseWriter, r *http.Request, id string, db *dynamodb.Client, table string) {
	result, err := db.GetItem(r.Context(), &dynamodb.GetItemInput{
		TableName: &table,
		Key:       map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: id}},
	})
	if err != nil {
		http.Error(w, "get failed", http.StatusInternalServerError)
		return
	}
	if result.Item == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var item Item
	for k, v := range result.Item {
		if av, ok := v.(*types.AttributeValueMemberS); ok {
			switch k {
			case "id":
				item.ID = av.Value
			case "name":
				item.Name = av.Value
			case "description":
				item.Description = av.Value
			}
		}
	}
	json.NewEncoder(w).Encode(item)
}

func deleteItem(w http.ResponseWriter, r *http.Request, id string, db *dynamodb.Client, table string) {
	db.DeleteItem(r.Context(), &dynamodb.DeleteItemInput{
		TableName: &table,
		Key:       map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: id}},
	})
	w.WriteHeader(http.StatusNoContent)
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%s-%d", hex.EncodeToString(b), time.Now().UnixMilli())
}

func awsStr(s string) *string { return &s }

func awsInt32(v int32) *int32 { return &v }
