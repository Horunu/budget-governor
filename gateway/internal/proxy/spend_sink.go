package proxy

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// SpendSink is the write destination for spend events. The gateway's
// SpendWriter batches events and flushes them through a sink; local
// dev uses Postgres, the AWS deploy uses DynamoDB (see
// deploy/aws/terraform/dynamodb.tf). A MultiSpendSink fans a batch out
// to several sinks for dual-write during migration.
type SpendSink interface {
	WriteBatch(ctx context.Context, batch []SpendEvent) error
}

// PostgresSpendSink keeps the original single-INSERT batch write path.
type PostgresSpendSink struct{ DB *sql.DB }

func (s *PostgresSpendSink) WriteBatch(ctx context.Context, batch []SpendEvent) error {
	const cols = 11
	values := make([]string, 0, len(batch))
	args := make([]interface{}, 0, len(batch)*cols)
	for i, e := range batch {
		base := i * cols
		placeholders := make([]string, cols)
		for j := 0; j < cols; j++ {
			placeholders[j] = fmt.Sprintf("$%d", base+j+1)
		}
		values = append(values, "("+strings.Join(placeholders, ",")+")")

		var agentID, taskID interface{}
		if e.AgentID != "" {
			agentID = e.AgentID
		}
		if e.TaskID != "" {
			taskID = e.TaskID
		}
		args = append(args, e.TenantID, agentID, taskID, e.Model, e.TokensIn, e.TokensOut, e.CostUSD, e.TraceID, e.Decision, e.DecisionReason, e.CreatedAt)
	}
	query := fmt.Sprintf(`
		INSERT INTO spend_events
			(tenant_id, agent_id, task_id, model, tokens_in, tokens_out, cost_usd, trace_id, decision, decision_reason, created_at)
		VALUES %s
	`, strings.Join(values, ","))
	_, err := s.DB.ExecContext(ctx, query, args...)
	return err
}

// DynamoSpendSink writes each event as one item keyed by
// (tenant_id, ts#trace_id) so the primary access pattern -- a tenant's
// spend in a time window -- is a Query with a BETWEEN condition on the
// sort key. BatchWriteItem accepts at most 25 items per call, so larger
// batches are chunked.
type DynamoSpendSink struct {
	Client    *dynamodb.Client
	TableName string
}

func (s *DynamoSpendSink) WriteBatch(ctx context.Context, batch []SpendEvent) error {
	for start := 0; start < len(batch); start += 25 {
		end := start + 25
		if end > len(batch) {
			end = len(batch)
		}
		writes := make([]types.WriteRequest, 0, end-start)
		for _, e := range batch[start:end] {
			item := map[string]types.AttributeValue{
				"tenant_id":       &types.AttributeValueMemberS{Value: e.TenantID},
				"ts_trace":        &types.AttributeValueMemberS{Value: e.CreatedAt.UTC().Format(time.RFC3339Nano) + "#" + e.TraceID},
				"model":           &types.AttributeValueMemberS{Value: e.Model},
				"tokens_in":       &types.AttributeValueMemberN{Value: strconv.Itoa(e.TokensIn)},
				"tokens_out":      &types.AttributeValueMemberN{Value: strconv.Itoa(e.TokensOut)},
				"cost_usd":        &types.AttributeValueMemberN{Value: strconv.FormatFloat(e.CostUSD, 'f', -1, 64)},
				"decision":        &types.AttributeValueMemberS{Value: e.Decision},
				"decision_reason": &types.AttributeValueMemberS{Value: e.DecisionReason},
				// DynamoDB TTL: expire records after 90 days to keep the
				// table in the always-free tier (shortcut: 90d retention,
				// extend by raising this offset once you need more history).
				"ttl": &types.AttributeValueMemberN{Value: strconv.FormatInt(e.CreatedAt.Add(90*24*time.Hour).Unix(), 10)},
			}
			if e.AgentID != "" {
				item["agent_id"] = &types.AttributeValueMemberS{Value: e.AgentID}
			}
			if e.TaskID != "" {
				item["task_id"] = &types.AttributeValueMemberS{Value: e.TaskID}
			}
			writes = append(writes, types.WriteRequest{PutRequest: &types.PutRequest{Item: item}})
		}
		out, err := s.Client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{s.TableName: writes},
		})
		if err != nil {
			return err
		}
		// One retry on throttled items; anything still unprocessed after
		// that is returned so the caller logs via the spend_write_dropped
		// path rather than looping here on the hot flush goroutine.
		if unproc, ok := out.UnprocessedItems[s.TableName]; ok && len(unproc) > 0 {
			retry, err := s.Client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
				RequestItems: map[string][]types.WriteRequest{s.TableName: unproc},
			})
			if err != nil {
				return err
			}
			if r, ok := retry.UnprocessedItems[s.TableName]; ok && len(r) > 0 {
				return fmt.Errorf("dynamodb: %d items still unprocessed after retry", len(r))
			}
		}
	}
	return nil
}

// MultiSpendSink fans a batch out to each wrapped sink and returns the
// first error so one broken backend never silently loses events on the
// others.
type MultiSpendSink struct{ Sinks []SpendSink }

func (s *MultiSpendSink) WriteBatch(ctx context.Context, batch []SpendEvent) error {
	var firstErr error
	for _, sink := range s.Sinks {
		if err := sink.WriteBatch(ctx, batch); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Compile-time check that the AWS config package stays in the module
// graph even when no sink ends up using it at runtime, so `go mod tidy`
// does not drop it from go.sum.
var _ = aws.ToString
