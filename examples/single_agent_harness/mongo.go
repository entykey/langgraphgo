package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type MsgUI struct {
	Role    string `bson:"role"    json:"role"`
	Content string `bson:"content" json:"content"`
}

type ConversationDoc struct {
	SessionID      string      `bson:"session_id"`
	Name           string      `bson:"name"`
	Status         string      `bson:"status"`
	LastStopReason string      `bson:"last_stop_reason"`
	EndReason      string      `bson:"end_reason"      json:"end_reason"`
	LastRequestID  string      `bson:"last_request_id"`
	Interrupted    bool        `bson:"interrupted"`
	UIMessages     []MsgUI     `bson:"ui_messages"`
	DSMessages     []dsChatMsg `bson:"ds_messages"`
	CreatedAt      time.Time   `bson:"created_at"`
	UpdatedAt      time.Time   `bson:"updated_at"`
}

type SessionPreview struct {
	SessionID string    `json:"session_id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
	Preview   string    `json:"preview"`
}

var (
	_mongoClient *mongo.Client
	_mongoColl   *mongo.Collection
	mongoOn      bool
)

func initMongo() {
	uri := getEnv("MONGO_URI", "")
	if uri == "" {
		fmt.Fprintln(os.Stderr, "[mongo] MONGO_URI not set — conversation persistence disabled")
		return
	}
	dbName := getEnv("MONGO_DB", "sah")

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mongo] connect error: %v\n", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		fmt.Fprintf(os.Stderr, "[mongo] ping error: %v\n", err)
		_ = client.Disconnect(context.Background())
		return
	}

	_mongoClient = client
	_mongoColl = client.Database(dbName).Collection("sah_conversations")
	mongoOn = true

	ensureMongoIndexes()
	mongoCrashRecovery()

	fmt.Printf("[mongo] connected — %s/%s/sah_conversations\n", uri, dbName)
}

func shutdownMongo() {
	if _mongoClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = _mongoClient.Disconnect(ctx)
}

func ensureMongoIndexes() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	indexes := []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "session_id", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		{
			Keys: bson.D{{Key: "updated_at", Value: -1}},
		},
	}
	if _, err := _mongoColl.Indexes().CreateMany(ctx, indexes); err != nil {
		fmt.Fprintf(os.Stderr, "[mongo] index creation: %v\n", err)
	}
}

func mongoCrashRecovery() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := _mongoColl.UpdateMany(ctx,
		bson.M{"status": "generating"},
		bson.M{"$set": bson.M{
			"status":           "idle",
			"last_stop_reason": "interrupted",
			"interrupted":      true,
			"updated_at":       time.Now(),
		}},
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mongo] crash recovery error: %v\n", err)
		return
	}
	if res.ModifiedCount > 0 {
		fmt.Printf("[mongo] crash recovery: reset %d interrupted session(s)\n", res.ModifiedCount)
	}
}

// ── Session loading ───────────────────────────────────────────────────────────

type sessionLoad struct {
	Name       string
	DSMessages []dsChatMsg
	Exists     bool
	Status     string
}

// loadSessionForTurn loads name + ds_messages + status from MongoDB.
func loadSessionForTurn(sessionID string) sessionLoad {
	if !mongoOn {
		fmt.Printf("[mongo:load] mongoOn=false, skip\n")
		return sessionLoad{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var doc ConversationDoc
	err := _mongoColl.FindOne(ctx,
		bson.M{"session_id": sessionID},
		options.FindOne().SetProjection(bson.M{"name": 1, "ds_messages": 1, "status": 1}),
	).Decode(&doc)
	if err != nil {
		fmt.Printf("[mongo:load] session=%s not found: %v\n", sessionID[:8], err)
		return sessionLoad{}
	}
	fmt.Printf("[mongo:load] session=%s found name=%q ds_msgs=%d status=%s\n", sessionID[:8], doc.Name, len(doc.DSMessages), doc.Status)
	return sessionLoad{Name: doc.Name, DSMessages: doc.DSMessages, Exists: true, Status: doc.Status}
}

// isDuplicate returns true if reqID matches the last processed request for this session.
func isDuplicate(sessionID, reqID string) bool {
	if !mongoOn || reqID == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var doc struct {
		LastRequestID string `bson:"last_request_id"`
	}
	err := _mongoColl.FindOne(ctx,
		bson.M{"session_id": sessionID},
		options.FindOne().SetProjection(bson.M{"last_request_id": 1}),
	).Decode(&doc)
	if err != nil {
		return false
	}
	return doc.LastRequestID == reqID
}

// ── Write operations ──────────────────────────────────────────────────────────

// upsertUserTurn saves user message + ds_messages, marks session as generating.
// name is only used when creating a new session (setOnInsert).
func upsertUserTurn(sessionID, name, reqID, userMsg string, dsMessages []dsChatMsg) error {
	if !mongoOn {
		fmt.Printf("[mongo:upsertUser] mongoOn=false, skip\n")
		return nil
	}
	fmt.Printf("[mongo:upsertUser] session=%s name=%q ds_msgs=%d userMsg_len=%d\n",
		sessionID[:8], name, len(dsMessages), len(userMsg))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Now()
	res, err := _mongoColl.UpdateOne(ctx,
		bson.M{"session_id": sessionID},
		bson.M{
			"$push": bson.M{"ui_messages": MsgUI{Role: "user", Content: userMsg}},
			"$set": bson.M{
				"status":          "generating",
				"last_request_id": reqID,
				"ds_messages":     dsMessages,
				"updated_at":      now,
			},
			"$setOnInsert": bson.M{
				"session_id": sessionID,
				"name":       name,
				"created_at": now,
			},
		},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		fmt.Printf("[mongo:upsertUser] ERROR: %v\n", err)
		return err
	}
	fmt.Printf("[mongo:upsertUser] ok matched=%d modified=%d upserted=%v\n",
		res.MatchedCount, res.ModifiedCount, res.UpsertedID)
	return nil
}

// upsertAssistantTurn saves assistant response + final ds_messages, marks session idle/ended.
func upsertAssistantTurn(sessionID, finalText string, finalDSMsgs []dsChatMsg, stopReason, endReason string) error {
	if !mongoOn {
		fmt.Printf("[mongo:upsertAssist] mongoOn=false, skip\n")
		return nil
	}
	fmt.Printf("[mongo:upsertAssist] session=%s stop=%s answer_len=%d ds_msgs=%d\n",
		sessionID[:8], stopReason, len(finalText), len(finalDSMsgs))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := "idle"
	if stopReason == "conversation_ended" {
		status = "ended"
	}
	setFields := bson.M{
		"status":           status,
		"last_stop_reason": stopReason,
		"ds_messages":      finalDSMsgs,
		"updated_at":       time.Now(),
	}
	if endReason != "" {
		setFields["end_reason"] = endReason
	}
	update := bson.M{"$set": setFields}
	if finalText != "" {
		update["$push"] = bson.M{"ui_messages": MsgUI{Role: "model", Content: finalText}}
	}
	res, err := _mongoColl.UpdateOne(ctx, bson.M{"session_id": sessionID}, update)
	if err != nil {
		fmt.Printf("[mongo:upsertAssist] ERROR: %v\n", err)
		return err
	}
	fmt.Printf("[mongo:upsertAssist] ok matched=%d modified=%d\n", res.MatchedCount, res.ModifiedCount)
	return nil
}

// ── REST helpers ──────────────────────────────────────────────────────────────

func listSessions(limit int) ([]SessionPreview, error) {
	if !mongoOn {
		fmt.Printf("[mongo:list] mongoOn=false, returning empty\n")
		return nil, nil
	}
	fmt.Printf("[mongo:list] listing up to %d sessions\n", limit)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cursor, err := _mongoColl.Find(ctx, bson.M{},
		options.Find().
			SetSort(bson.D{{Key: "updated_at", Value: -1}}).
			SetLimit(int64(limit)).
			SetProjection(bson.M{
				"session_id":  1,
				"name":        1,
				"status":      1,
				"updated_at":  1,
				"ui_messages": bson.M{"$slice": 1},
			}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var result []SessionPreview
	for cursor.Next(ctx) {
		var doc ConversationDoc
		if err := cursor.Decode(&doc); err != nil {
			continue
		}
		preview := ""
		if len(doc.UIMessages) > 0 {
			preview = truncate(doc.UIMessages[0].Content, 100)
		}
		result = append(result, SessionPreview{
			SessionID: doc.SessionID,
			Name:      doc.Name,
			Status:    doc.Status,
			UpdatedAt: doc.UpdatedAt,
			Preview:   preview,
		})
	}
	fmt.Printf("[mongo:list] returned %d sessions err=%v\n", len(result), cursor.Err())
	return result, cursor.Err()
}

func getSessionDoc(sessionID string) (*ConversationDoc, error) {
	if !mongoOn {
		return nil, nil
	}
	fmt.Printf("[mongo:getSession] session=%s\n", sessionID[:8])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var doc ConversationDoc
	if err := _mongoColl.FindOne(ctx, bson.M{"session_id": sessionID}).Decode(&doc); err != nil {
		fmt.Printf("[mongo:getSession] ERROR: %v\n", err)
		return nil, err
	}
	fmt.Printf("[mongo:getSession] found name=%q ui_msgs=%d status=%s\n", doc.Name, len(doc.UIMessages), doc.Status)
	return &doc, nil
}

func deleteSessionDoc(sessionID string) error {
	if !mongoOn {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := _mongoColl.DeleteOne(ctx, bson.M{"session_id": sessionID})
	return err
}

func renameSessionDoc(sessionID, name string) error {
	if !mongoOn {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := _mongoColl.UpdateOne(ctx,
		bson.M{"session_id": sessionID},
		bson.M{"$set": bson.M{"name": name, "updated_at": time.Now()}},
	)
	return err
}
