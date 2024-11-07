package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.temporal.io/api/cloud/cloudservice/v1"
	"go.temporal.io/api/cloud/usage/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/types/known/timestamppb"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	client.CloudOperationsClient
}

type StoreCalculatedNamespaceCostsWorkflowInput struct {
	Date           Date
	StoreUsageData bool
	TaskQueueName  string
}

type StoreCalculatedNamespaceCostsWorkflowOutput struct{}

type Date struct {
	Year  int
	Month int
	Day   int
}

type StoreNamespaceCostsRequest struct {
	Date           Date
	NamespaceCosts []NamespaceCost
}

type StoreUsageRequest struct {
	Date         Date
	RecordGroups []*usage.RecordGroup
}

type NamespaceCost struct {
	Namespace              string
	ActionsCostUSD         float64
	ActiveStorageCostUSD   float64
	RetainedStorageCostUSD float64
	TotalCostUSD           float64
}

type StoreCalculatedNamespaceCostsInput struct {
	Date Date
}

type StoreCalculatedNamespaceCostsOutput struct{}

type StoreUsageInput struct {
	Date Date
}
type StoreUsageOutput struct{}

type Storage interface {
	StoreNamespaceCosts(*StoreNamespaceCostsRequest) error
	StoreUsage(*StoreUsageRequest) error
}

type CostCalculator interface {
	Calculate([]*usage.RecordGroup) ([]NamespaceCost, error)
}

type ActivityHandler struct {
	calc CostCalculator
	cli  *Client
	s    Storage
}

func (h *ActivityHandler) StoreUsage(ctx context.Context, in *StoreUsageInput) (*StoreUsageOutput, error) {
	rgs, err := h.getRecordGroups(ctx, in.Date)
	if err != nil {
		return nil, err
	}
	err = h.s.StoreUsage(&StoreUsageRequest{
		Date:         in.Date,
		RecordGroups: rgs,
	})
	return &StoreUsageOutput{}, err
}

func (h *ActivityHandler) getRecordGroups(ctx context.Context, in Date) ([]*usage.RecordGroup, error) {
	endDayUTCMidnight := time.Date(in.Year, time.Month(in.Month), in.Day, 0, 0, 0, 0,
		time.UTC)
	startDayUTCMidnight := endDayUTCMidnight.AddDate(0, 0, -1)
	// Perform first request to check if there are additional pages
	var resp *cloudservice.GetUsageResponse
	var err error
	req := &cloudservice.GetUsageRequest{
		StartTimeInclusive: timestamppb.New(startDayUTCMidnight),
		EndTimeExclusive:   timestamppb.New(endDayUTCMidnight),
		PageSize:           0,
		PageToken:          "",
	}
	resp, err = h.cli.CloudService().GetUsage(ctx, req)
	if err != nil {
		return nil, err
	}
	allRecordGroups := make([]*usage.RecordGroup, 0)
	for _, s := range resp.GetSummaries() {
		allRecordGroups = append(allRecordGroups, s.GetRecordGroups()...)
	}
	if resp.GetNextPageToken() != "" {
		// Iterate through remaining pages
		for resp.GetNextPageToken() != "" && req.PageToken != resp.GetNextPageToken() {
			req.PageToken = resp.GetNextPageToken()
			resp, err = h.cli.CloudService().GetUsage(ctx, req, nil)
			if err != nil {
				return nil, err
			}
			for _, s := range resp.GetSummaries() {
				allRecordGroups = append(allRecordGroups, s.GetRecordGroups()...)
			}
		}
	}
	return allRecordGroups, nil
}

func (h *ActivityHandler) StoreCalculatedNamespaceCosts(ctx context.Context,
	in *StoreCalculatedNamespaceCostsInput) (*StoreCalculatedNamespaceCostsOutput, error) {
	rgs, err := h.getRecordGroups(ctx, in.Date)
	if err != nil {
		return nil, err
	}
	namespaceCosts, err := h.calc.Calculate(rgs)
	if err != nil {
		return nil, err
	}
	storeRequest := &StoreNamespaceCostsRequest{
		Date:           in.Date,
		NamespaceCosts: namespaceCosts,
	}
	return &StoreCalculatedNamespaceCostsOutput{}, h.s.StoreNamespaceCosts(storeRequest)
}

func NewActivityHandler(calc CostCalculator, cli *Client, s Storage) (*ActivityHandler, error) {
	if cli.CloudService() == nil {
		return nil, errors.New("cloud service client required and missing")
	}
	if s == nil {
		return nil, errors.New("storage required and missing")
	}
	if calc == nil {
		return nil, errors.New("cost calculator required and missing")
	}
	return &ActivityHandler{
		calc: calc,
		cli:  cli,
		s:    s,
	}, nil
}

type WorkflowHandler struct{}

func (h *WorkflowHandler) StoreCalculatedNamespaceCosts(ctx workflow.Context,
	in *StoreCalculatedNamespaceCostsWorkflowInput) (*StoreCalculatedNamespaceCostsWorkflowOutput, error) {
	ao := workflow.ActivityOptions{
		TaskQueue:           in.TaskQueueName,
		StartToCloseTimeout: 5 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    5 * time.Minute,
		},
	}
	actCtx := workflow.WithActivityOptions(ctx, ao)
	if in.StoreUsageData {
		err := workflow.ExecuteActivity(actCtx,
			"StoreUsage",
			&StoreUsageInput{
				Date: Date{
					Year:  in.Date.Year,
					Month: in.Date.Month,
					Day:   in.Date.Day,
				},
			},
		).Get(ctx, nil)
		if err != nil {
			return nil, err
		}
	}
	err := workflow.ExecuteActivity(actCtx,
		"StoreCalculatedNamespaceCosts",
		&StoreCalculatedNamespaceCostsInput{
			Date: Date{
				Year:  in.Date.Year,
				Month: in.Date.Month,
				Day:   in.Date.Day,
			},
		},
	).Get(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &StoreCalculatedNamespaceCostsWorkflowOutput{}, nil
}

func NewWorkflowHandler() (*WorkflowHandler, error) {
	return &WorkflowHandler{}, nil
}

// NaiveCostCalculator performs a simplistic cost calculation where all costs are calculated at a given rate per
// record unit type. It does not take support costs, included actions, credit discounts, or other costs such as addons
// into consideration. If accounting for these costs is needed, develop a custom cost calculator fit for the needs of
// your organization.
type NaiveCostCalculator struct {
	CostPerMillionActionsUSD    float64
	CostPerActiveGigabyteHour   float64
	CostPerRetainedGigabyteHour float64
}

func (c NaiveCostCalculator) Calculate(recordGroups []*usage.RecordGroup) ([]NamespaceCost, error) {
	out := make([]NamespaceCost, 0)
	for _, rg := range recordGroups {
		nc := NamespaceCost{}
		for _, gb := range rg.GetGroupBys() {
			if gb.Key == usage.GROUP_BY_KEY_NAMESPACE {
				nc.Namespace = gb.Value
			}
		}
		for _, r := range rg.GetRecords() {
			if r.Type == usage.RECORD_TYPE_ACTIONS {
				oneMillionth := decimal.NewFromFloat(0.000001)
				costPerMillionActions := decimal.NewFromFloat(c.CostPerMillionActionsUSD)
				costPerAction := costPerMillionActions.Mul(oneMillionth)
				actions := decimal.NewFromFloat(r.GetValue())
				v, _ := actions.Mul(costPerAction).Float64()
				nc.ActionsCostUSD = v
			}
			if r.Type == usage.RECORD_TYPE_ACTIVE_STORAGE {
				if r.Unit == usage.RECORD_UNIT_BYTE_SECONDS {
					storageByteSeconds := decimal.NewFromFloat(r.GetValue())
					bytesPerGB := decimal.NewFromInt(1_000_000_000)
					storageGigabyteSeconds := storageByteSeconds.Div(bytesPerGB)
					secondsPerHour := decimal.NewFromInt(3600)
					storageGigabyteHours := storageGigabyteSeconds.Div(secondsPerHour)
					costPerGigabyteHour := decimal.NewFromFloat(c.CostPerActiveGigabyteHour)
					v, _ := storageGigabyteHours.Mul(costPerGigabyteHour).Float64()
					nc.ActiveStorageCostUSD = v
				}
			}
			if r.Type == usage.RECORD_TYPE_RETAINED_STORAGE {
				if r.Unit == usage.RECORD_UNIT_BYTE_SECONDS {
					storageByteSeconds := decimal.NewFromFloat(r.GetValue())
					bytesPerGB := decimal.NewFromInt(1_000_000_000)
					storageGigabyteSeconds := storageByteSeconds.Div(bytesPerGB)
					secondsPerHour := decimal.NewFromInt(3600)
					storageGigabyteHours := storageGigabyteSeconds.Div(secondsPerHour)
					costPerGigabyteHour := decimal.NewFromFloat(c.CostPerRetainedGigabyteHour)
					v, _ := storageGigabyteHours.Mul(costPerGigabyteHour).Float64()
					nc.RetainedStorageCostUSD = v
				}
			}
		}
		nc.TotalCostUSD = nc.ActionsCostUSD + nc.ActiveStorageCostUSD + nc.RetainedStorageCostUSD
		out = append(out, nc)
	}
	return out, nil
}

func NewNaiveCostCalculator(costPerMillionActionsUSD float64, costPerActiveGigabyteHour float64,
	costPerRetainedGigabyteHour float64) (NaiveCostCalculator, error) {
	return NaiveCostCalculator{
		CostPerMillionActionsUSD:    costPerMillionActionsUSD,
		CostPerRetainedGigabyteHour: costPerRetainedGigabyteHour,
		CostPerActiveGigabyteHour:   costPerActiveGigabyteHour,
	}, nil
}

// InMemoryStorage is provided an example that fulfills the Storage interface. Develop a storage mechanism that meets
// the needs of your organization.
type InMemoryStorage struct {
	Costs map[string]NamespaceCost
	Usage []*StoreUsageRequest
}

func (s InMemoryStorage) StoreNamespaceCosts(request *StoreNamespaceCostsRequest) error {
	for _, nc := range request.NamespaceCosts {
		s.Costs[nc.Namespace] = nc
	}
	return nil
}

func (s InMemoryStorage) StoreUsage(request *StoreUsageRequest) error {
	s.Usage = append(s.Usage, request)
	return nil
}

var (
	_                       client.CloudOperationsClient = &Client{}
	TemporalCloudAPIVersion                              = "2024-10-01-00"
)

func NewConnectionWithAPIKey(addrStr string, allowInsecure bool, apiKey string) (*Client, error) {
	var cClient client.CloudOperationsClient
	var err error
	cClient, err = client.DialCloudOperationsClient(context.Background(), client.CloudOperationsClientOptions{
		Version:     TemporalCloudAPIVersion,
		Credentials: client.NewAPIKeyStaticCredentials(apiKey),
		DisableTLS:  allowInsecure,
		HostPort:    addrStr,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect `%s`: %v", client.DefaultHostPort, err)
	}

	return &Client{cClient}, nil
}

func main() {
	apiKey := strings.TrimSpace(os.Getenv("TEMPORAL_CLOUD_OPS_API_KEY"))
	tlsKeyPath := strings.TrimSpace(os.Getenv("TEMPORAL_CLOUD_NAMESPACE_TLS_KEY"))
	tlsCertPath := strings.TrimSpace(os.Getenv("TEMPORAL_CLOUD_NAMESPACE_TLS_CERT"))
	ns := strings.TrimSpace(os.Getenv("NAMESPACE"))
	actionsCostRaw := strings.TrimSpace(os.Getenv("COST_PER_MILLION_ACTIONS"))
	actionsCost, err := strconv.ParseFloat(actionsCostRaw, 64)
	if err != nil {
		panic("Unable to parse COST_PER_MILLION_ACTIONS: " + err.Error())
	}
	activeStorageCostRaw := strings.TrimSpace(os.Getenv("COST_PER_GBHR_ACTIVE"))
	activeStorageCost, err := strconv.ParseFloat(activeStorageCostRaw, 64)
	if err != nil {
		panic("Unable to parse COST_PER_GBHR_ACTIVE: " + err.Error())
	}
	retainedStorageCostRaw := strings.TrimSpace(os.Getenv("COST_PER_GBHR_RETAINED"))
	retainedStorageCost, err := strconv.ParseFloat(retainedStorageCostRaw, 64)
	if err != nil {
		panic("Unable to parse COST_PER_GBHR_RETAINED: " + err.Error())
	}
	if apiKey == "" {
		panic("TEMPORAL_CLOUD_OPS_API_KEY required and missing")
	}
	if tlsKeyPath == "" {
		panic("TEMPORAL_CLOUD_NAMESPACE_TLS_KEY required and missing")
	}
	if tlsCertPath == "" {
		panic("TEMPORAL_CLOUD_NAMESPACE_TLS_CERT required and missing")
	}
	if ns == "" {
		panic("NAMESPACE required and missing")
	}
	if actionsCost <= 0 {
		panic("Invalid value for COST_PER_MILLION_ACTIONS")
	}
	if activeStorageCost <= 0 {
		panic("Invalid value for COST_PER_GBHR_ACTIVE")
	}
	if retainedStorageCost < 0 {
		panic("Invalid value for COST_PER_GBHR_RETAINED")
	}
	// Instantiate Cloud Ops Client
	opsClient, err := NewConnectionWithAPIKey("saas-api.tmprl.cloud:443", false, apiKey)
	if err != nil {
		panic(err)
	}
	defer opsClient.Close()
	// Instantiate Temporal Cloud Client
	cert, err := tls.LoadX509KeyPair(tlsCertPath, tlsKeyPath)
	if err != nil {
		panic(err)
	}
	clientOptions := client.Options{
		HostPort:  ns + ".tmprl.cloud:7233",
		Namespace: ns,
		ConnectionOptions: client.ConnectionOptions{
			TLS: &tls.Config{Certificates: []tls.Certificate{cert}},
		},
		DataConverter: converter.GetDefaultDataConverter(),
	}
	tcClient, err := client.Dial(clientOptions)
	defer tcClient.Close()
	storage := InMemoryStorage{
		Costs: make(map[string]NamespaceCost),
	}
	calculator, err := NewNaiveCostCalculator(actionsCost, activeStorageCost, retainedStorageCost)
	if err != nil {
		log.Fatalln("Unable to initialize cost calculator")
	}
	taskQueueName := "usage"
	ah, err := NewActivityHandler(calculator, opsClient, storage)
	if err != nil {
		log.Fatalln("Unable to initialize activity handler", err)
	}
	wh, err := NewWorkflowHandler()
	if err != nil {
		log.Fatalln("unable to init workflow handler", err)
	}
	w := worker.New(tcClient, taskQueueName, worker.Options{})
	w.RegisterWorkflow(wh.StoreCalculatedNamespaceCosts)
	w.RegisterActivity(ah.StoreCalculatedNamespaceCosts)
	w.RegisterActivity(ah.StoreUsage)
	err = w.Start()
	if err != nil {
		log.Fatalln("Unable to start worker", err)
	}
	opts := client.StartWorkflowOptions{
		ID:        uuid.New().String(),
		TaskQueue: taskQueueName,
	}
	y, m, d := time.Now().In(time.UTC).Date()
	workflowInput := StoreCalculatedNamespaceCostsWorkflowInput{
		Date: Date{
			Year:  y,
			Month: int(m),
			Day:   d,
		},
		StoreUsageData: true,
	}
	run, err := tcClient.ExecuteWorkflow(context.Background(), opts, "StoreCalculatedNamespaceCosts", workflowInput)
	if err != nil {
		log.Fatalln("unable to start workflow", opts)
	}
	err = run.Get(context.Background(), nil)
	if err != nil {
		log.Fatalln("unable to get workflow", opts)
	}
	w.Stop()
	for _, nc := range storage.Costs {
		fmt.Println("NAMESPACE:", nc.Namespace)
		fmt.Println("TOTAL COST:", decimal.NewFromFloat(nc.TotalCostUSD).Round(9))
		fmt.Println("ACTIONS COST:", decimal.NewFromFloat(nc.ActionsCostUSD).Round(9))
		fmt.Println("ACTIVE STORAGE COST:", decimal.NewFromFloat(nc.ActiveStorageCostUSD).Round(9))
		fmt.Println("RETAINED STORAGE COST:", decimal.NewFromFloat(nc.RetainedStorageCostUSD).Round(9))
	}
}
