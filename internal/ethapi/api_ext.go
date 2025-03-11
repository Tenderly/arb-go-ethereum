package ethapi

import (
	"context"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
)

func (s *BlockChainAPI) StylusGetAsm(ctx context.Context, codeHash string) (hexutil.Bytes, error) {
	key, _, err := decodeHash(codeHash)
	if err != nil {
		return nil, err
	}

	db, _, err := s.b.StateAndHeaderByNumber(ctx, 1)
	if err != nil {
		return nil, err
	}

	asm, err := db.Database().ActivatedAsm(rawdb.LocalTarget(), key)
	if err != nil {
		return nil, err
	}

	return asm, nil
}

func (s *BlockChainAPI) StylusGetModule(ctx context.Context, codeHash string) (hexutil.Bytes, error) {
	key, _, err := decodeHash(codeHash)
	if err != nil {
		return nil, err
	}

	db, _, err := s.b.StateAndHeaderByNumber(ctx, 1)
	if err != nil {
		return nil, err
	}

	asm, err := db.Database().ActivatedAsm(rawdb.TargetWavm, key)
	if err != nil {
		return nil, err
	}

	return asm, nil
}
