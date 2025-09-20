package sip

type nonInviteClientTransactionState int

const (
	nonInviteClientTransactionStateTrying nonInviteClientTransactionState = iota
	nonInviteClientTransactionStateProceeding
	nonInviteClientTransactionStateCompleted
)

type nonInviteClientTransaction struct {
	*transactionData
	state      nonInviteClientTransactionState
	serverTxID string
}

func newNonInviteClientTransaction(data *transactionData, serverTxID string) clientTransaction {
	return &nonInviteClientTransaction{
		transactionData: data,
		state:           nonInviteClientTransactionStateTrying,
		serverTxID:      serverTxID,
	}
}

func (t *nonInviteClientTransaction) data() *transactionData {
	return t.transactionData
}

func (t *nonInviteClientTransaction) onReceiveResponse(status int) bool {
	if status < 200 {
		t.state = nonInviteClientTransactionStateProceeding
		return false
	}
	t.state = nonInviteClientTransactionStateCompleted
	return true
}

func (t *nonInviteClientTransaction) serverID() string {
	return t.serverTxID
}
