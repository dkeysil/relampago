package relampago

import "time"

type Wallet interface {
	Kind() string
	GetInfo() (WalletInfo, error)

	CreateInvoice(InvoiceParams) (InvoiceData, error)
	GetInvoiceStatus(string) (InvoiceStatus, error)
	PaidInvoicesStream() (<-chan InvoiceStatus, error)

	MakePayment(PaymentParams) (PaymentData, error)
	GetPaymentStatus(string) (PaymentStatus, error)
	PaymentsStream() (<-chan PaymentStatus, error)
}

type WalletInfo struct {
	Balance int64 `json:"balance"`
}

type InvoiceParams struct {
	Msatoshi        int64          `json:"msatoshi"`
	Description     string         `json:"description"`
	DescriptionHash []byte         `json:"descriptionHash"`
	Expiry          *time.Duration `json:"expiry"`
}

type InvoiceData struct {
	CheckingID string `json:"checkingID"`
	Preimage   string `json:"preimage"`
	Invoice    string `json:"invoice"`
}

type InvoiceStatus struct {
	CheckingID       string `json:"checkingID"`
	Exists           bool   `json:"exists"`
	Paid             bool   `json:"paid"`
	MSatoshiReceived int64  `json:"msatoshiReceived"`
}

type PaymentParams struct {
	Invoice      string `json:"invoice"`
	CustomAmount int64  `json:"customAmount"`
}

type PaymentData struct {
	CheckingID string `json:"checkingID"`
}

type Status string

const (
	Unknown    Status = "unknown"
	NeverTried Status = "never-tried"
	Pending    Status = "pending"
	Failed     Status = "failed"
	Complete   Status = "complete"
)

type PaymentStatus struct {
	CheckingID string `json:"checkingID"`
	Status     Status `json:"status"`
	FeePaid    int64  `json:"feePaid"`
	Preimage   string `json:"preimage"`
}
