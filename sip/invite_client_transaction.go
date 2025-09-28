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
		t.state = inviteClientTransactionStateTerminated
		return true
	}
	t.state = inviteClientTransactionStateCompleted
	return false
}

func (t *inviteClientTransaction) onTimeout() {
	if t == nil {
		return
	}
	t.state = inviteClientTransactionStateTerminated
}

func (t *inviteClientTransaction) serverID() string {
	return t.serverTxID
}
