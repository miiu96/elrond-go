package txsFee

import (
	"math/big"
	"testing"

	"github.com/ElrondNetwork/elrond-go/core/vmcommon"

	"github.com/ElrondNetwork/elrond-go/integrationTests/vm"
	"github.com/ElrondNetwork/elrond-go/process"
	"github.com/stretchr/testify/require"
)

func TestRelayedMoveBalanceShouldWork(t *testing.T) {
	testContext := vm.CreatePreparedTxProcessorWithVMs(t, true)
	defer testContext.Close()

	relayerAddr := []byte("12345678901234567890123456789033")
	sndAddr := []byte("12345678901234567890123456789012")
	rcvAddr := []byte("12345678901234567890123456789022")

	senderNonce := uint64(0)
	senderBalance := big.NewInt(0)
	gasPrice := uint64(10)
	gasLimit := uint64(100)

	_, _ = vm.CreateAccount(testContext.Accounts, sndAddr, 0, senderBalance)
	_, _ = vm.CreateAccount(testContext.Accounts, relayerAddr, 0, big.NewInt(3000))

	// gas consumed = 50
	userTx := vm.CreateTransaction(senderNonce, big.NewInt(100), sndAddr, rcvAddr, gasPrice, gasLimit, []byte("aaaa"))

	rtxData := prepareRelayerTxData(userTx)
	rTxGasLimit := 1 + gasLimit + uint64(len(rtxData))
	rtx := vm.CreateTransaction(0, userTx.Value, relayerAddr, sndAddr, gasPrice, rTxGasLimit, rtxData)

	_, err := testContext.TxProcessor.ProcessTransaction(rtx)
	require.Nil(t, err)
	require.Nil(t, testContext.GetLatestError())

	_, err = testContext.Accounts.Commit()
	require.Nil(t, err)

	//check relayer balance
	// 3000 - value(100) - gasLimit(275)*gasPrice(10) = 2850
	expectedBalanceRelayer := big.NewInt(150)
	vm.TestAccount(t, testContext.Accounts, relayerAddr, 1, expectedBalanceRelayer)

	// check balance inner tx sender
	vm.TestAccount(t, testContext.Accounts, sndAddr, 1, big.NewInt(0))

	// check balance inner tx receiver
	vm.TestAccount(t, testContext.Accounts, rcvAddr, 0, big.NewInt(100))

	// check accumulated fees
	accumulatedFees := testContext.TxFeeHandler.GetAccumulatedFees()
	require.Equal(t, big.NewInt(2750), accumulatedFees)
}

func TestRelayedMoveBalanceInvalidGasLimitShouldConsumeGas(t *testing.T) {
	testContext := vm.CreatePreparedTxProcessorWithVMs(t, true)
	defer testContext.Close()

	relayerAddr := []byte("12345678901234567890123456789033")
	sndAddr := []byte("12345678901234567890123456789012")
	rcvAddr := []byte("12345678901234567890123456789022")

	_, _ = vm.CreateAccount(testContext.Accounts, sndAddr, 0, big.NewInt(0))
	userTx := vm.CreateTransaction(0, big.NewInt(100), sndAddr, rcvAddr, 1, 100, []byte("aaaa"))

	_, _ = vm.CreateAccount(testContext.Accounts, relayerAddr, 0, big.NewInt(3000))

	rtxData := prepareRelayerTxData(userTx)
	rTxGasLimit := 2 + userTx.GasLimit + uint64(len(rtxData))
	rtx := vm.CreateTransaction(0, userTx.Value, relayerAddr, sndAddr, 1, rTxGasLimit, rtxData)

	_, err := testContext.TxProcessor.ProcessTransaction(rtx)
	require.Equal(t, process.ErrFailedTransaction, err)
	require.Nil(t, testContext.GetLatestError())

	_, err = testContext.Accounts.Commit()
	require.Nil(t, err)

	expectedBalanceRelayer := big.NewInt(2724)
	vm.TestAccount(t, testContext.Accounts, relayerAddr, 1, expectedBalanceRelayer)

	// check accumulated fees
	accumulatedFees := testContext.TxFeeHandler.GetAccumulatedFees()
	require.Equal(t, big.NewInt(276), accumulatedFees)
}

func TestRelayedMoveBalanceInvalidUserTxShouldConsumeGas(t *testing.T) {
	testContext := vm.CreatePreparedTxProcessorWithVMs(t, true)
	defer testContext.Close()

	relayerAddr := []byte("12345678901234567890123456789033")
	sndAddr := []byte("12345678901234567890123456789012")
	rcvAddr := []byte("12345678901234567890123456789022")

	_, _ = vm.CreateAccount(testContext.Accounts, sndAddr, 0, big.NewInt(0))
	userTx := vm.CreateTransaction(1, big.NewInt(100), sndAddr, rcvAddr, 1, 100, []byte("aaaa"))

	_, _ = vm.CreateAccount(testContext.Accounts, relayerAddr, 0, big.NewInt(3000))

	rtxData := prepareRelayerTxData(userTx)
	rTxGasLimit := 1 + userTx.GasLimit + uint64(len(rtxData))
	rtx := vm.CreateTransaction(0, userTx.Value, relayerAddr, sndAddr, 1, rTxGasLimit, rtxData)

	retcode, err := testContext.TxProcessor.ProcessTransaction(rtx)
	require.Equal(t, vmcommon.UserError, retcode)
	require.Nil(t, testContext.GetLatestError())

	_, err = testContext.Accounts.Commit()
	require.Nil(t, err)

	expectedBalanceRelayer := big.NewInt(2721)
	vm.TestAccount(t, testContext.Accounts, relayerAddr, 1, expectedBalanceRelayer)

	// check accumulated fees
	accumulatedFees := testContext.TxFeeHandler.GetAccumulatedFees()
	require.Equal(t, big.NewInt(279), accumulatedFees)
}

func TestRelayedMoveBalanceInvalidUserTxValueShouldConsumeGas(t *testing.T) {
	testContext := vm.CreatePreparedTxProcessorWithVMs(t, true)
	defer testContext.Close()

	relayerAddr := []byte("12345678901234567890123456789033")
	sndAddr := []byte("12345678901234567890123456789012")
	rcvAddr := []byte("12345678901234567890123456789022")

	_, _ = vm.CreateAccount(testContext.Accounts, sndAddr, 0, big.NewInt(0))
	userTx := vm.CreateTransaction(0, big.NewInt(150), sndAddr, rcvAddr, 1, 100, []byte("aaaa"))

	_, _ = vm.CreateAccount(testContext.Accounts, relayerAddr, 0, big.NewInt(3000))

	rtxData := prepareRelayerTxData(userTx)
	rTxGasLimit := 1 + userTx.GasLimit + uint64(len(rtxData))
	rtx := vm.CreateTransaction(0, big.NewInt(100), relayerAddr, sndAddr, 1, rTxGasLimit, rtxData)

	retCode, err := testContext.TxProcessor.ProcessTransaction(rtx)
	require.Equal(t, vmcommon.UserError, retCode)
	require.Nil(t, testContext.GetLatestError())

	_, err = testContext.Accounts.Commit()
	require.Nil(t, err)

	expectedBalanceRelayer := big.NewInt(2725)
	vm.TestAccount(t, testContext.Accounts, relayerAddr, 1, expectedBalanceRelayer)

	// check accumulated fees
	accumulatedFees := testContext.TxFeeHandler.GetAccumulatedFees()
	require.Equal(t, big.NewInt(275), accumulatedFees)
}