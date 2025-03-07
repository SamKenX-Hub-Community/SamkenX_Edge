package polybft

import (
	"errors"
	"fmt"

	"github.com/0xPolygon/polygon-edge/consensus/polybft/validator"
	bolt "go.etcd.io/bbolt"
)

var (
	// bucket to store full validator set
	validatorSetBucket = []byte("fullValidatorSetBucket")
	// key of the full validator set in bucket
	fullValidatorSetKey = []byte("fullValidatorSet")
	// error returned if full validator set does not exists in db
	errNoFullValidatorSet = errors.New("full validator set not in db")
)

type StakeStore struct {
	db *bolt.DB
}

// initialize creates necessary buckets in DB if they don't already exist
func (s *StakeStore) initialize(tx *bolt.Tx) error {
	if _, err := tx.CreateBucketIfNotExists(validatorSetBucket); err != nil {
		return fmt.Errorf("failed to create bucket=%s: %w", string(epochsBucket), err)
	}

	return nil
}

// insertFullValidatorSet inserts full validator set to its bucket (or updates it if exists)
func (s *StakeStore) insertFullValidatorSet(fullValidatorSet validatorSetState) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		raw, err := fullValidatorSet.Marshal()
		if err != nil {
			return err
		}

		return tx.Bucket(validatorSetBucket).Put(fullValidatorSetKey, raw)
	})
}

// getFullValidatorSet returns full validator set from its bucket if exists
func (s *StakeStore) getFullValidatorSet() (validatorSetState, error) {
	var fullValidatorSet validatorSetState

	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(validatorSetBucket).Get(fullValidatorSetKey)
		if raw == nil {
			return errNoFullValidatorSet
		}

		return fullValidatorSet.Unmarshal(raw)
	})

	return fullValidatorSet, err
}

// GetValidators returns all validators regardless if they are active or not
func (s *StakeStore) GetAllValidators() (validator.AccountSet, error) {
	fullValidatorSet, err := s.getFullValidatorSet()
	if err != nil {
		return nil, err
	}

	return fullValidatorSet.Validators.getSorted(len(fullValidatorSet.Validators)), nil
}
