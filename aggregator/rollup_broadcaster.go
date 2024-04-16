package aggregator

import (
	"context"
	"errors"
	"time"

	"github.com/Layr-Labs/eigensdk-go/logging"
	"github.com/Layr-Labs/eigensdk-go/signerv2"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"

	opsetupdatereg "github.com/NethermindEth/near-sffl/contracts/bindings/SFFLOperatorSetUpdateRegistry"
	registryrollup "github.com/NethermindEth/near-sffl/contracts/bindings/SFFLRegistryRollup"
	"github.com/NethermindEth/near-sffl/core/chainio"
	"github.com/NethermindEth/near-sffl/core/config"
	"github.com/NethermindEth/near-sffl/core/types/messages"
)

const (
	GET_OPERATOR_SET_RETRIES        = 5
	GET_OPERATOR_SET_RETRY_INTERVAL = time.Millisecond * 500
)

type RollupBroadcasterer interface {
	BroadcastOperatorSetUpdate(ctx context.Context, message messages.OperatorSetUpdateMessage, signatureInfo registryrollup.RollupOperatorsSignatureInfo)
	GetErrorChan() <-chan error
}

type RollupBroadcaster struct {
	writers   []*RollupWriter
	logger    logging.Logger
	errorChan chan error
}

func NewRollupBroadcaster(
	ctx context.Context,
	avsReader chainio.AvsReaderer,
	avsSubscriber chainio.AvsSubscriberer,
	rollupsInfo map[uint32]config.RollupInfo,
	signerConfig signerv2.Config,
	address common.Address,
	logger logging.Logger,
) (*RollupBroadcaster, error) {
	writers := make([]*RollupWriter, 0, len(rollupsInfo))

	for id, info := range rollupsInfo {
		writer, err := NewRollupWriter(ctx, id, info, signerConfig, address, logger)
		if err != nil {
			logger.Error("Couldn't create RollupWriter", "chainId", id, "err", err)
			return nil, err
		}

		writers = append(writers, writer)
	}

	broadcaster := &RollupBroadcaster{
		writers:   writers,
		logger:    logger,
		errorChan: make(chan error),
	}

	mainnetNextOperatorSetUpdateId, err := avsReader.GetNextOperatorSetUpdateId(ctx)
	if err != nil {
		logger.Error("Error fetching operator set update id", "err", err)
		return nil, err
	}

	if mainnetNextOperatorSetUpdateId == 0 {
		go broadcaster.initializeRollupOperatorSetsOnUpdate(ctx, avsReader, avsSubscriber)
	} else {
		for _, writer := range broadcaster.writers {
			go broadcaster.tryInitializeRollupOperatorSet(ctx, writer, mainnetNextOperatorSetUpdateId, avsReader, avsSubscriber)
		}
	}

	return broadcaster, nil
}

func (b *RollupBroadcaster) initializeRollupOperatorSetsOnUpdate(ctx context.Context, avsReader chainio.AvsReaderer, avsSubscriber chainio.AvsSubscriberer) {
	var operatorSetUpdatedChan chan *opsetupdatereg.ContractSFFLOperatorSetUpdateRegistryOperatorSetUpdatedAtBlock

	avsSubscriber.SubscribeToOperatorSetUpdates(operatorSetUpdatedChan)

	for {
		select {
		case <-ctx.Done():
			return
		case event := <-operatorSetUpdatedChan:
			b.logger.Info("Received operator set update", "id", event.Id)

			operators, err := avsReader.GetOperatorSetById(ctx, event.Id)
			if err != nil {
				b.errorChan <- err
				continue
			}

			convertedOperators := make([]registryrollup.RollupOperatorsOperator, len(operators))
			for i, op := range operators {
				convertedOperators[i] = registryrollup.RollupOperatorsOperator{
					Pubkey: registryrollup.BN254G1Point{X: op.Pubkey.X, Y: op.Pubkey.Y},
					Weight: op.Weight,
				}
			}

			for _, writer := range b.writers {
				go func(writer *RollupWriter) {
					err := writer.InitializeOperatorSet(ctx, convertedOperators, event.Id)
					if err != nil {
						b.errorChan <- err
					}
				}(writer)
			}
		}
	}
}

func (b *RollupBroadcaster) tryInitializeRollupOperatorSet(ctx context.Context, writer *RollupWriter, mainnetNextOperatorSetUpdateId uint64, avsReader chainio.AvsReaderer, avsSubscriber chainio.AvsSubscriberer) {
	nextOperatorUpdateId, err := writer.sfflRegistryRollup.NextOperatorUpdateId(&bind.CallOpts{})
	if err != nil {
		b.logger.Error("Error fetching NextOperatorUpdateId", "err", err)
		b.errorChan <- err
		return
	}

	if nextOperatorUpdateId != 0 {
		return
	}

	b.logger.Info("Operator set not initialized yet", "rollupId", writer.rollupId, "mainnetNextOperatorSetUpdateId", mainnetNextOperatorSetUpdateId)

	operators, err := b.tryGetOperatorSetById(ctx, avsReader, mainnetNextOperatorSetUpdateId-1)
	if err != nil {
		b.logger.Error("Error fetching operator set", "err", err)
		b.errorChan <- err
		return
	}

	convertedOperators := make([]registryrollup.RollupOperatorsOperator, len(operators))
	for i, op := range operators {
		convertedOperators[i] = registryrollup.RollupOperatorsOperator{
			Pubkey: registryrollup.BN254G1Point{X: op.Pubkey.X, Y: op.Pubkey.Y},
			Weight: op.Weight,
		}
	}

	err = writer.InitializeOperatorSet(ctx, convertedOperators, mainnetNextOperatorSetUpdateId-1)
	if err != nil {
		b.logger.Error("Error initializing operator set", "err", err)
		b.errorChan <- err
	}
}

func (b *RollupBroadcaster) BroadcastOperatorSetUpdate(ctx context.Context, message messages.OperatorSetUpdateMessage, signatureInfo registryrollup.RollupOperatorsSignatureInfo) {
	go func() {
		for _, writer := range b.writers {
			select {
			case <-ctx.Done():
				return

			default:
				err := writer.UpdateOperatorSet(ctx, message, signatureInfo)
				if err != nil {
					b.errorChan <- err
				}
			}
		}
	}()
}

func (b *RollupBroadcaster) GetErrorChan() <-chan error {
	return b.errorChan
}

func (b *RollupBroadcaster) tryGetOperatorSetById(ctx context.Context, avsReader chainio.AvsReaderer, operatorSetUpdateId uint64) ([]opsetupdatereg.RollupOperatorsOperator, error) {
	for i := 0; i < GET_OPERATOR_SET_RETRIES; i++ {
		operators, err := avsReader.GetOperatorSetById(ctx, operatorSetUpdateId)

		if err == nil {
			return operators, nil
		}

		b.errorChan <- err

		select {
		case <-ctx.Done():
			b.logger.Info("Context canceled")
			return nil, errors.New("failed to fetch operator set early")

		case <-time.After(GET_OPERATOR_SET_RETRY_INTERVAL):
			continue
		}
	}

	return nil, errors.New("failed to fetch operator set after retries")
}