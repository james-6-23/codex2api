package database

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type CodexIdentityEpoch struct {
	RootKey        string `json:"root_key,omitempty"`
	Generation     uint64 `json:"generation"`
	Segment        string `json:"segment,omitempty"`
	MappingVersion string `json:"mapping_version,omitempty"`
}

func validCodexIdentityEpoch(epoch CodexIdentityEpoch) bool {
	_, rootError := hex.DecodeString(epoch.RootKey)
	validRoot := epoch.RootKey == "" || (len(epoch.RootKey) == 24 || len(epoch.RootKey) == 64) && rootError == nil && epoch.RootKey == strings.ToLower(epoch.RootKey)
	return validRoot && (epoch.Segment == "" || ValidSessionOperationKey(epoch.Segment)) &&
		(epoch.MappingVersion == "" || epoch.MappingVersion == CodexIdentityMappingUUIDv7)
}

func (db *DB) PublishCodexIdentityEpoch(ctx context.Context, identityKey string, next CodexIdentityEpoch) error {
	if !ValidSessionOperationKey(identityKey) || !validCodexIdentityEpoch(next) {
		return errors.New("invalid codex identity epoch")
	}
	var snapshot string
	if err := db.conn.QueryRowContext(ctx, `SELECT state FROM codex_identity_epochs WHERE identity_key=$1`, identityKey).Scan(&snapshot); err == nil {
		var existing CodexIdentityEpoch
		if err := json.Unmarshal([]byte(snapshot), &existing); err != nil {
			return err
		}
		if existing == next {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return db.withWriteTx(ctx, func(transaction *sql.Tx) error {
		payload, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `INSERT INTO codex_identity_epochs(identity_key,state) VALUES ($1,$2) ON CONFLICT(identity_key) DO UPDATE SET state=codex_identity_epochs.state`, identityKey, string(payload)); err != nil {
			return err
		}
		var raw string
		if err := transaction.QueryRowContext(ctx, `SELECT state FROM codex_identity_epochs WHERE identity_key=$1`, identityKey).Scan(&raw); err != nil {
			return err
		}
		var existing CodexIdentityEpoch
		if err := json.Unmarshal([]byte(raw), &existing); err != nil {
			return err
		}
		if existing.Generation > next.Generation || existing.Generation == next.Generation && existing.Segment != next.Segment || existing.RootKey != "" && next.RootKey != "" && existing.RootKey != next.RootKey {
			return ErrSessionOwnerConflict
		}
		if existing.Generation == next.Generation && existing.MappingVersion != next.MappingVersion {
			return ErrSessionOwnerConflict
		}
		if next.RootKey == "" {
			next.RootKey = existing.RootKey
			payload, _ = json.Marshal(next)
		}
		_, err = transaction.ExecContext(ctx, `UPDATE codex_identity_epochs SET state=$2 WHERE identity_key=$1`, identityKey, string(payload))
		return err
	})
}

func (db *DB) ReadCodexIdentityReference(ctx context.Context, referenceKey, identityKey string) (epoch CodexIdentityEpoch, found bool, bound bool, err error) {
	if !ValidSessionOperationKey(referenceKey) || !ValidSessionOperationKey(identityKey) {
		return epoch, false, false, errors.New("invalid codex identity reference")
	}
	var raw string
	err = db.conn.QueryRowContext(ctx, `SELECT state FROM codex_identity_references WHERE reference_key=$1`, referenceKey).Scan(&raw)
	bound = err == nil
	if errors.Is(err, sql.ErrNoRows) {
		err = db.conn.QueryRowContext(ctx, `SELECT state FROM codex_identity_epochs WHERE identity_key=$1`, identityKey).Scan(&raw)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return epoch, false, false, nil
	}
	if err != nil {
		return epoch, false, false, err
	}
	if err := json.Unmarshal([]byte(raw), &epoch); err != nil {
		return epoch, false, false, err
	}
	if !validCodexIdentityEpoch(epoch) {
		return epoch, false, false, errors.New("invalid stored codex identity epoch")
	}
	return epoch, true, bound, nil
}

func (db *DB) ClaimCodexIdentityReference(ctx context.Context, referenceKey string, epoch CodexIdentityEpoch) error {
	if !ValidSessionOperationKey(referenceKey) || !validCodexIdentityEpoch(epoch) {
		return errors.New("invalid codex identity reference")
	}
	var snapshot string
	if err := db.conn.QueryRowContext(ctx, `SELECT state FROM codex_identity_references WHERE reference_key=$1`, referenceKey).Scan(&snapshot); err == nil {
		var existing CodexIdentityEpoch
		if err := json.Unmarshal([]byte(snapshot), &existing); err != nil {
			return err
		}
		if existing != epoch {
			return ErrCodexIdentityConflict
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return db.withWriteTx(ctx, func(transaction *sql.Tx) error {
		payload, err := json.Marshal(epoch)
		if err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `INSERT INTO codex_identity_references(reference_key,state) VALUES ($1,$2) ON CONFLICT(reference_key) DO NOTHING`, referenceKey, string(payload)); err != nil {
			return err
		}
		var raw string
		if err := transaction.QueryRowContext(ctx, `SELECT state FROM codex_identity_references WHERE reference_key=$1`, referenceKey).Scan(&raw); err != nil {
			return err
		}
		var existing CodexIdentityEpoch
		if err := json.Unmarshal([]byte(raw), &existing); err != nil {
			return err
		}
		if existing != epoch {
			return ErrCodexIdentityConflict
		}
		return nil
	})
}
