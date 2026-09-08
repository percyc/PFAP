package api

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/pfap/lab/internal/model"
)

func transactionRPCAmount(kind, value string) (string, error) {
	parsed, ok := amount(value)
	bits := 64
	if kind == "public" {
		bits = 256
	}
	if !ok || parsed.Sign() <= 0 || parsed.BitLen() > bits {
		return "", errors.New("交易金额不是有效范围内的正整数，未调用节点 RPC")
	}
	return "0x" + parsed.Text(16), nil
}

const amountDecodeConclusion = "核验结论：节点在 JSON-RPC 金额参数解码时拒绝调用，未进入 Mint/Redeem 方法；已标记未执行失败，未重发交易。"
const amountDecodePrefix = "执行结果待核验；不会自动重发，相关节点继续保留交易占用。 RPC 未返回可确认的交易哈希：Error: invalid argument 0: json: cannot unmarshal hex string without 0x prefix into Go struct field SendTxArgs.value of type *hexutil.Big"

var amountDecodeStack = regexp.MustCompile(`^\n    at web3\.js:[0-9]+:[0-9]+\n    at web3\.js:[0-9]+:[0-9]+\n    at web3\.js:[0-9]+:[0-9]+\n    at <anonymous>:1:1$`)

// Only the exact generated one-call expression and RPC decoder error qualify.
// rpc/json.go rejects positional-argument decoding before invoking the handler.
// Never apply this to Transfer, arbitrary console code, timeouts or state errors.
func amountDecodeNeverExecuted(tx model.Transaction) bool {
	if (tx.Type != "mint" && tx.Type != "redeem") || tx.Status != "unknown" || tx.ExecutionStage != "submit" || tx.FromNode == "" ||
		tx.Hash != "" || tx.Receipt != "" || tx.BlockNumber != "" || tx.ReceiptStatus != "" || !tx.BroadcastAt.IsZero() || !tx.ConfirmedAt.IsZero() || !tx.ReadyAt.IsZero() ||
		tx.ProvingAt.IsZero() || tx.SubmissionAttemptedAt.IsZero() || tx.ToNode != "" {
		return false
	}
	value, ok := amount(tx.Value)
	if !ok || value.Sign() <= 0 || strings.HasPrefix(tx.Value, "0x") {
		return false
	}
	method := "Mint"
	if tx.Type == "redeem" {
		method = "Redeem"
	}
	parts := strings.SplitN(tx.Command, ": ", 2)
	if len(parts) != 2 || !regexp.MustCompile(`^node-[0-9]+$`).MatchString(parts[0]) || parts[1] != "eth.send"+method+"Transaction({from:eth.accounts[0],value:"+strconv.Quote(tx.Value)+"})" {
		return false
	}
	return strings.HasPrefix(tx.Error, amountDecodePrefix) && amountDecodeStack.MatchString(strings.TrimPrefix(tx.Error, amountDecodePrefix))
}
