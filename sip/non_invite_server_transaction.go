package sip

type nonInviteServerTransactionState int

const (
	nonInviteServerTransactionStateTrying nonInviteServerTransactionState = iota
	nonInviteServerTransactionStateProceeding
	nonInviteServerTransactionStateCompleted
	nonInviteServerTransactionStateTerminated
)

type nonInviteServerTransaction struct {
	*transactionData
	state nonInviteServerTransactionState
}

func newNonInviteServerTransaction(data *transactionData) serverTransaction {
	return &nonInviteServerTransaction{
		transactionData: data,
		state:           nonInviteServerTransactionStateTrying,
	}
}

func (t *nonInviteServerTransaction) data() *transactionData {
	return t.transactionData
}

func (t *nonInviteServerTransaction) onSendResponse(status int) {
	if status < 200 {
		t.state = nonInviteServerTransactionStateProceeding
		return
	}
	t.state = nonInviteServerTransactionStateTerminated
}
