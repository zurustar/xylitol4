package sip

type inviteServerTransactionState int

const (
	inviteServerTransactionStateProceeding inviteServerTransactionState = iota
	inviteServerTransactionStateCompleted
	inviteServerTransactionStateTerminated
)

type inviteServerTransaction struct {
	*transactionData
	state inviteServerTransactionState
}

func newInviteServerTransaction(data *transactionData) serverTransaction {
	return &inviteServerTransaction{
		transactionData: data,
		state:           inviteServerTransactionStateProceeding,
	}
}

func (t *inviteServerTransaction) data() *transactionData {
	return t.transactionData
}

func (t *inviteServerTransaction) onSendResponse(status int) {
	if status < 200 {
		t.state = inviteServerTransactionStateProceeding
		return
	}
	if status < 300 {
		t.state = inviteServerTransactionStateTerminated
		return
	}
	t.state = inviteServerTransactionStateCompleted
}
