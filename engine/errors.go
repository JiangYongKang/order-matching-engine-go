package engine

import "fmt"

// RejectReason is a distinguishable rejection code.
type RejectReason string

const (
	RejectInvalidSide        RejectReason = "INVALID_SIDE"
	RejectInvalidOrderType   RejectReason = "INVALID_ORDER_TYPE"
	RejectInvalidQty         RejectReason = "INVALID_QTY"
	RejectInvalidPrice       RejectReason = "INVALID_PRICE"
	RejectEmptyOrderID       RejectReason = "EMPTY_ORDER_ID"
	RejectDuplicateOrderID   RejectReason = "DUPLICATE_ORDER_ID"
	RejectUnknownAccount     RejectReason = "UNKNOWN_ACCOUNT"
	RejectUnknownSymbol      RejectReason = "UNKNOWN_SYMBOL"
	RejectInsufficientFunds  RejectReason = "INSUFFICIENT_FUNDS"
	RejectInsufficientShares RejectReason = "INSUFFICIENT_SHARES"
	RejectSelfTrade          RejectReason = "SELF_TRADE"
	RejectRiskSingleOrder    RejectReason = "RISK_SINGLE_ORDER_LIMIT"
	RejectRiskGrossExposure  RejectReason = "RISK_GROSS_EXPOSURE_LIMIT"
	RejectRiskSymbolPosition RejectReason = "RISK_SYMBOL_POSITION_LIMIT"
	RejectRiskGlobalExposure RejectReason = "RISK_GLOBAL_EXPOSURE_LIMIT"
	RejectNoLiquidity        RejectReason = "NO_LIQUIDITY"
	RejectOrderNotFound      RejectReason = "ORDER_NOT_FOUND"
	RejectOrderClosed        RejectReason = "ORDER_NOT_ACTIVE"
	RejectInvalidReplace     RejectReason = "INVALID_REPLACE"
)

// RejectError carries a stable reason code plus a human-readable message.
type RejectError struct {
	Reason  RejectReason
	Message string
}

func (e *RejectError) Error() string {
	if e == nil {
		return ""
	}
	return string(e.Reason) + ": " + e.Message
}

func rejectf(reason RejectReason, format string, args ...interface{}) *RejectError {
	return &RejectError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}
