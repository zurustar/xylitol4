package sip

type inviteClientTransactionState int

const (
	inviteClientTransactionStateCalling inviteClientTransactionState = iota
	inviteClientTransactionStateProceeding
	inviteClientTransactionStateCompleted
	inviteClientTransactionStateTerminated
)

type inviteClientTransaction struct {
	*transactionData
	state      inviteClientTransactionState
	serverTxID string
}

func newInviteClientTransaction(data *transactionData, serverTxID string) clientTransaction {
	return &inviteClientTransaction{
		transactionData: data,
		state:           inviteClientTransactionStateCalling,
		serverTxID:      serverTxID,
	}
}

func (t *inviteClientTransaction) data() *transactionData {
	return t.transactionData
}

func (t *inviteClientTransaction) onReceiveResponse(status int) bool {
	if status < 200 {
		t.state = inviteClientTransactionStateProceeding
		return false
	}
	if status < 300 {
		t.state = inviteClientTransactionStateCompleted
	} else {
		t.state = inviteClientTransactionStateTerminated
	}
	return true
}

func (t *inviteClientTransaction) serverID() string {
	return t.serverTxID
}
