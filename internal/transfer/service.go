package transfer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type TransferService struct {
	db *sql.DB
}

type CreateTransferRequest struct {
	FromAccountID  uuid.UUID              `json:"from_account_id"`
	ToAccount      map[string]interface{} `json:"to_account"`
	AmountCents    int64                  `json:"amount_cents"`
	Currency       string                 `json:"currency"`
	Rail           string                 `json:"rail"` // "stripe", "ach", "wire"
	IdempotencyKey string                 `json:"idempotency_key"`
	InitiatedBy    uuid.UUID              `json:"initiated_by"`
	Metadata       map[string]interface{} `json:"metadata"`
}

type Transfer struct {
	ID              uuid.UUID              `json:"id"`
	IdempotencyKey  string                 `json:"idempotency_key"`
	FromAccountID   uuid.UUID              `json:"from_account_id"`
	ToAccount       map[string]interface{} `json:"to_account"`
	AmountCents     int64                  `json:"amount_cents"`
	Currency        string                 `json:"currency"`
	Rail            string                 `json:"rail"`
	Status          string                 `json:"status"`
	ExternalID      string                 `json:"external_id"`
	InitiatedBy     uuid.UUID              `json:"initiated_by"`
	CreatedAt       time.Time              `json:"created_at"`
	UpdatedAt       time.Time              `json:"updated_at"`
	Metadata        map[string]interface{} `json:"metadata"`
}

func NewTransferService(db *sql.DB) *TransferService {
	return &TransferService{db: db}
}

// InitiateTransfer creates a new transfer and handles rail-specific logic
func (ts *TransferService) InitiateTransfer(ctx context.Context, req CreateTransferRequest) (*Transfer, error) {
	// Check idempotency
	existingTransfer, err := ts.getTransferByIdempotencyKey(ctx, req.IdempotencyKey)
	if err == nil && existingTransfer != nil {
		return existingTransfer, nil // Return existing transfer if already processed
	}

	transferID := uuid.New()
	metadata, _ := json.Marshal(req.Metadata)
	toAccount, _ := json.Marshal(req.ToAccount)

	query := `
		INSERT INTO transfers (
			id, idempotency_key, from_account_id, to_account, amount_cents, 
			currency, rail, status, initiated_by, metadata
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, idempotency_key, from_account_id, to_account, amount_cents, currency, 
		          rail, status, external_id, initiated_by, created_at, updated_at, metadata
	`

	transfer := &Transfer{}
	var toAccountJSON, metadataJSON []byte

	err = ts.db.QueryRowContext(ctx, query,
		transferID, req.IdempotencyKey, req.FromAccountID, string(toAccount),
		req.AmountCents, req.Currency, req.Rail, "pending", req.InitiatedBy, string(metadata),
	).Scan(
		&transfer.ID, &transfer.IdempotencyKey, &transfer.FromAccountID,
		&toAccountJSON, &transfer.AmountCents, &transfer.Currency,
		&transfer.Rail, &transfer.Status, &transfer.ExternalID,
		&transfer.InitiatedBy, &transfer.CreatedAt, &transfer.UpdatedAt, &metadataJSON,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to create transfer: %w", err)
	}

	json.Unmarshal(toAccountJSON, &transfer.ToAccount)
	json.Unmarshal(metadataJSON, &transfer.Metadata)

	// Place hold on account
	err = ts.placeHold(ctx, req.FromAccountID, transferID, req.AmountCents)
	if err != nil {
		return nil, fmt.Errorf("failed to place hold: %w", err)
	}

	// Record initial event
	ts.recordEvent(ctx, transferID, "transfer_initiated", nil)

	return transfer, nil
}

// placeHold creates a hold on the account to prevent overdrafts
func (ts *TransferService) placeHold(ctx context.Context, accountID, transferID uuid.UUID, amountCents int64) error {
	holdID := uuid.New()
	expiresAt := time.Now().Add(7 * 24 * time.Hour) // Hold expires in 7 days

	query := `
		INSERT INTO holds (id, account_id, transfer_id, amount_cents, expires_at, status)
		VALUES ($1, $2, $3, $4, $5, 'active')
	`

	_, err := ts.db.ExecContext(ctx, query, holdID, accountID, transferID, amountCents, expiresAt)
	return err
}

// CompleteTransfer marks transfer as completed and releases the hold
func (ts *TransferService) CompleteTransfer(ctx context.Context, transferID uuid.UUID, externalID string) error {
	query := `
		UPDATE transfers 
		SET status = 'completed', external_id = $1, updated_at = now()
		WHERE id = $2
	`

	_, err := ts.db.ExecContext(ctx, query, externalID, transferID)
	if err != nil {
		return fmt.Errorf("failed to complete transfer: %w", err)
	}

	// Record event
	ts.recordEvent(ctx, transferID, "transfer_completed", map[string]interface{}{
		"external_id": externalID,
	})

	return nil
}

// FailTransfer marks transfer as failed and releases the hold
func (ts *TransferService) FailTransfer(ctx context.Context, transferID uuid.UUID, failureReason string) error {
	query := `
		UPDATE transfers 
		SET status = 'failed', updated_at = now()
		WHERE id = $1
	`

	_, err := ts.db.ExecContext(ctx, query, transferID)
	if err != nil {
		return fmt.Errorf("failed to mark transfer as failed: %w", err)
	}

	// Release hold
	ts.releaseHold(ctx, transferID)

	// Record event
	ts.recordEvent(ctx, transferID, "transfer_failed", map[string]interface{}{
		"reason": failureReason,
	})

	return nil
}

// releaseHold cancels the hold on an account
func (ts *TransferService) releaseHold(ctx context.Context, transferID uuid.UUID) error {
	query := `
		UPDATE holds 
		SET status = 'released', updated_at = now()
		WHERE transfer_id = $1 AND status = 'active'
	`

	_, err := ts.db.ExecContext(ctx, query, transferID)
	return err
}

// getTransferByIdempotencyKey retrieves an existing transfer by idempotency key
func (ts *TransferService) getTransferByIdempotencyKey(ctx context.Context, key string) (*Transfer, error) {
	query := `
		SELECT id, idempotency_key, from_account_id, to_account, amount_cents, currency, 
		       rail, status, external_id, initiated_by, created_at, updated_at, metadata
		FROM transfers 
		WHERE idempotency_key = $1
	`

	transfer := &Transfer{}
	var toAccountJSON, metadataJSON []byte

	err := ts.db.QueryRowContext(ctx, query, key).Scan(
		&transfer.ID, &transfer.IdempotencyKey, &transfer.FromAccountID,
		&toAccountJSON, &transfer.AmountCents, &transfer.Currency,
		&transfer.Rail, &transfer.Status, &transfer.ExternalID,
		&transfer.InitiatedBy, &transfer.CreatedAt, &transfer.UpdatedAt, &metadataJSON,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get transfer: %w", err)
	}

	json.Unmarshal(toAccountJSON, &transfer.ToAccount)
	json.Unmarshal(metadataJSON, &transfer.Metadata)

	return transfer, nil
}

// recordEvent logs a transfer event
func (ts *TransferService) recordEvent(ctx context.Context, transferID uuid.UUID, eventType string, payload map[string]interface{}) {
	payloadJSON, _ := json.Marshal(payload)
	query := `
		INSERT INTO transfer_events (transfer_id, event_type, payload)
		VALUES ($1, $2, $3)
	`
	ts.db.ExecContext(ctx, query, transferID, eventType, string(payloadJSON))
}

// GetTransfer retrieves a transfer by ID
func (ts *TransferService) GetTransfer(ctx context.Context, transferID uuid.UUID) (*Transfer, error) {
	query := `
		SELECT id, idempotency_key, from_account_id, to_account, amount_cents, currency, 
		       rail, status, external_id, initiated_by, created_at, updated_at, metadata
		FROM transfers 
		WHERE id = $1
	`

	transfer := &Transfer{}
	var toAccountJSON, metadataJSON []byte

	err := ts.db.QueryRowContext(ctx, query, transferID).Scan(
		&transfer.ID, &transfer.IdempotencyKey, &transfer.FromAccountID,
		&toAccountJSON, &transfer.AmountCents, &transfer.Currency,
		&transfer.Rail, &transfer.Status, &transfer.ExternalID,
		&transfer.InitiatedBy, &transfer.CreatedAt, &transfer.UpdatedAt, &metadataJSON,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to get transfer: %w", err)
	}

	json.Unmarshal(toAccountJSON, &transfer.ToAccount)
	json.Unmarshal(metadataJSON, &transfer.Metadata)

	return transfer, nil
}
