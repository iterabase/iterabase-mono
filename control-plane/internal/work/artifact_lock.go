package work

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// lockClearedArtifactTx acquires the artifact's lifecycle lock in share mode
// and only then evaluates artifact.is_cleared in a separate statement, so the
// clearance it reports reflects any deletion or withdrawal that committed
// while the lock was awaited (section 16). The lock is held until the
// transaction ends, so deletion cannot begin until the reference this
// transaction creates is visible to its active-reference check.
func lockClearedArtifactTx(ctx context.Context, tx pgx.Tx, artifactID string) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, `SELECT true FROM artifact.artifacts WHERE id=$1 FOR SHARE`, artifactID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cleared bool
	if err := tx.QueryRow(ctx, `SELECT artifact.is_cleared($1)`, artifactID).Scan(&cleared); err != nil {
		return false, err
	}
	return cleared, nil
}

// isArtifactUnavailable reports the work.artifact_links serialization
// trigger's refusal: the artifact entered deletion before the reference could
// be acquired.
func isArtifactUnavailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55000"
}
