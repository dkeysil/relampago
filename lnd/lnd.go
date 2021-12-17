package lnd

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	rp "github.com/fiatjaf/relampago"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/prometheus/common/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	macaroon "gopkg.in/macaroon.v2"
	"io"
	"io/ioutil"
	"strconv"
	"time"
)

var PaymentPollInterval = 3 * time.Second

type Params struct {
	Host              string
	CertPath          string
	AdminMacaroonPath string
}

type LndWallet struct {
	Params

	Conn      *grpc.ClientConn
	Lightning lnrpc.LightningClient
	Router    routerrpc.RouterClient

	invoiceStatusListeners []chan rp.InvoiceStatus
	paymentStatusListeners []chan rp.PaymentStatus
}

func Start(params Params) (*LndWallet, error) {
	var dialOpts []grpc.DialOption

	// TLS
	tls, err := credentials.NewClientTLSFromFile(params.CertPath, "")
	if err != nil {
		return nil, err
	}
	dialOpts = append(dialOpts, grpc.WithTransportCredentials(tls))

	// Macaroon Auth
	macBytes, err := ioutil.ReadFile(params.AdminMacaroonPath)
	if err != nil {
		return nil, err
	}
	m := &macaroon.Macaroon{}
	err = m.UnmarshalBinary(macBytes)
	if err != nil {
		return nil, err
	}
	creds, err := macaroons.NewMacaroonCredential(m)
	if err != nil {
		return nil, err
	}
	dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(creds))
	dialOpts = append(dialOpts, grpc.WithBlock())

	// Connect
	conn, err := grpc.Dial(params.Host, dialOpts...)
	if err != nil {
		return nil, err
	}
	ln := lnrpc.NewLightningClient(conn)
	router := routerrpc.NewRouterClient(conn)

	l := &LndWallet{
		Params:    params,
		Conn:      conn,
		Lightning: ln,
		Router:    router,
	}
	l.StartStreams()

	return l, nil
}

func (l *LndWallet) StartStreams() {
	go l.startPaymentsStream()
	go l.startInvoicesStream()
}

// Compile time check to ensure that LndWallet fully implements rp.Wallet
var _ rp.Wallet = (*LndWallet)(nil)

func (l *LndWallet) GetInfo() (rp.WalletInfo, error) {
	res, err := l.Lightning.ChannelBalance(context.Background(), &lnrpc.ChannelBalanceRequest{})
	if err != nil {
		return rp.WalletInfo{}, fmt.Errorf("error calling ChannelBalance: %w", err)
	}
	return rp.WalletInfo{
		Balance: int64(res.LocalBalance.Sat),
	}, nil
}

func (l *LndWallet) CreateInvoice(params rp.InvoiceParams) (rp.InvoiceData, error) {
	invoice, err := l.Lightning.AddInvoice(context.Background(), &lnrpc.Invoice{
		Memo:            params.Description,
		DescriptionHash: params.DescriptionHash,
		ValueMsat:       params.Msatoshi,
		Expiry:          int64(params.Expiry.Seconds()),
	})
	if err != nil {
		return rp.InvoiceData{}, fmt.Errorf("error calling AddInvoice: %w", err)
	}

	// LookupInvoice to get the preimage since AddInvoice only returns the hash
	res, err := l.Lightning.LookupInvoice(context.Background(), &lnrpc.PaymentHash{RHash: invoice.RHash})
	if err != nil {
		return rp.InvoiceData{}, fmt.Errorf("error calling LookupInvoice: %w", err)
	}
	return rp.InvoiceData{
		CheckingID: hex.EncodeToString(res.RHash),
		Preimage:   hex.EncodeToString(res.RPreimage),
		Invoice:    res.PaymentRequest,
	}, nil
}

func (l *LndWallet) GetInvoiceStatus(checkingID string) (rp.InvoiceStatus, error) {
	rHash, err := hex.DecodeString(checkingID)
	if err != nil {
		return rp.InvoiceStatus{}, fmt.Errorf("invalid checkingID: %w", err)
	}
	res, err := l.Lightning.LookupInvoice(context.Background(), &lnrpc.PaymentHash{RHash: rHash})
	if err != nil || res == nil {
		return rp.InvoiceStatus{
			CheckingID:       checkingID,
			Exists:           false,
			Paid:             false,
			MSatoshiReceived: 0,
		}, nil
	}
	return rp.InvoiceStatus{
		CheckingID:       checkingID,
		Exists:           true,
		Paid:             res.State == lnrpc.Invoice_SETTLED,
		MSatoshiReceived: res.AmtPaidMsat,
	}, nil
}

func (l *LndWallet) MakePayment(params rp.PaymentParams) (rp.PaymentData, error) {
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: params.Invoice,
	}
	if params.CustomAmount != 0 {
		req.AmtMsat = params.CustomAmount
	}
	stream, err := l.Router.SendPaymentV2(context.Background(), req)
	if err != nil {
		return rp.PaymentData{}, fmt.Errorf("error calling SendPaymentV2: %w", err)
	}
	res, err := stream.Recv()
	if err != nil {
		return rp.PaymentData{}, fmt.Errorf("error getting response from SendPaymentV2: %w", err)
	}

	return rp.PaymentData{
		CheckingID: fmt.Sprintf("%d", res.PaymentIndex),
	}, nil
}

func (l *LndWallet) GetPaymentStatus(checkingID string) (rp.PaymentStatus, error) {
	payIndex, err := strconv.ParseUint(checkingID, 10, 64)
	if err != nil {
		return rp.PaymentStatus{}, fmt.Errorf("error parsing checkingID: %w", err)
	}
	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: true,
		IndexOffset:       payIndex - 1,
		MaxPayments:       1,
		Reversed:          false,
	}
	res, err := l.Lightning.ListPayments(context.Background(), req)
	if err != nil {
		return rp.PaymentStatus{}, fmt.Errorf("error calling ListPayments: %w", err)
	}
	if len(res.Payments) == 0 {
		return rp.PaymentStatus{}, fmt.Errorf("payment with ID %s not found", checkingID)
	}

	return l.paymentToPaymentStatus(res.Payments[0]), nil
}

func (l *LndWallet) paymentToPaymentStatus(payment *lnrpc.Payment) rp.PaymentStatus {
	status := rp.PaymentStatus{
		CheckingID: fmt.Sprintf("%d", payment.PaymentIndex),
		Status:     rp.Unknown,
		FeePaid:    0,
		Preimage:   "",
	}

	switch payment.Status {
	case lnrpc.Payment_IN_FLIGHT:
		status.Status = rp.Pending
		return status
	case lnrpc.Payment_FAILED:
		if len(payment.Htlcs) == 0 {
			status.Status = rp.NeverTried
		} else {
			status.Status = rp.Failed
		}
		return status
	case lnrpc.Payment_SUCCEEDED:
		status.Status = rp.Complete
		status.FeePaid = payment.FeeMsat
		status.Preimage = payment.PaymentPreimage
		return status
	default:
		return status
	}
}

func (l *LndWallet) PaidInvoicesStream() (<-chan rp.InvoiceStatus, error) {
	listener := make(chan rp.InvoiceStatus)
	l.invoiceStatusListeners = append(l.invoiceStatusListeners, listener)
	return listener, nil
}

func (l *LndWallet) PaymentsStream() (<-chan rp.PaymentStatus, error) {
	listener := make(chan rp.PaymentStatus)
	l.paymentStatusListeners = append(l.paymentStatusListeners, listener)
	return listener, nil
}

func (l *LndWallet) startInvoicesStream() {
	stream, err := l.Lightning.SubscribeInvoices(context.Background(), &lnrpc.InvoiceSubscription{})
	if err != nil {
		log.Fatalf("Failed to SubscribeInvoices: %v", err)
	}
	for {
		res, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Errorf("Error receiving invoice event: %v", err)
		}

		if res.State != lnrpc.Invoice_SETTLED {
			continue // Only notify for paid invoices
		}
		for _, listener := range l.invoiceStatusListeners {
			go func(listener chan rp.InvoiceStatus) {
				listener <- rp.InvoiceStatus{
					CheckingID:       hex.EncodeToString(res.RHash),
					Exists:           true,
					Paid:             res.State == lnrpc.Invoice_SETTLED,
					MSatoshiReceived: res.AmtPaidMsat,
				}
			}(listener)
		}
	}
}

func (l *LndWallet) startPaymentsStream() {
	latest, err := l.getLatestPayment()
	var latestIndex uint64 = 0
	if err == nil {
		latestIndex = latest.PaymentIndex
	}

	// There is no way to subscribe to payment updates, so we must poll
	for {
		time.Sleep(PaymentPollInterval)
		res, err := l.Lightning.ListPayments(context.Background(), &lnrpc.ListPaymentsRequest{
			IncludeIncomplete: false,
			IndexOffset:       latestIndex,
		})
		if err != nil {
			log.Errorf("Error getting payments: %v", err)
		}
		if len(res.Payments) == 0 {
			continue
		}
		for _, listener := range l.paymentStatusListeners {
			for _, payment := range res.Payments {
				go func(listener chan rp.PaymentStatus, payment *lnrpc.Payment) {
					listener <- l.paymentToPaymentStatus(payment)
				}(listener, payment)
			}
		}
		latestIndex = res.LastIndexOffset
	}
}

func (l *LndWallet) getLatestPayment() (*lnrpc.Payment, error) {
	res, err := l.Lightning.ListPayments(context.Background(), &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: false,
		IndexOffset:       0,
		MaxPayments:       1,
		Reversed:          true,
	})
	if err != nil {
		return nil, err
	}
	if len(res.Payments) == 0 {
		return nil, errors.New("no payments found")
	}
	return res.Payments[0], nil
}
