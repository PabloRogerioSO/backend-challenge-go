package postgres

import (
	"context"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
)

// FindProcessedReversal returns the PROCESSED REFUND/ROLLBACK that already
// reversed the transaction referenceID, or ErrNotFound when there is none.
func (r *WageringRepo) FindProcessedReversal(ctx context.Context, q Querier, referenceID string) (*wagering.WagerTransaction, error) {
	return queryWager(ctx, q,
		`SELECT `+wagerColumns+`
		   FROM wager_transactions
		  WHERE resolved_reference_transaction_id = $1
		    AND status = 'PROCESSED'
		    AND kind IN ('REFUND', 'ROLLBACK')
		  ORDER BY created_at
		  LIMIT 1`,
		"processed reversal of "+referenceID, referenceID)
}
