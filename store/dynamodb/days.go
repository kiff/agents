package dynamodb

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/kiff/agents/usage"
)

// Days keeps usage.Days' rows in the agents table: one item per row, under
// its day's partition, so a day's rows are one query.
type Days struct {
	db    API
	table string
}

func NewDays(db API, table string) *Days { return &Days{db: db, table: table} }

var _ usage.Days = (*Days)(nil)

func dayPK(day string) string { return key("DAY", day) }

func daySK(k usage.DayKey) string { return key("", k.Provider, k.Agent, k.Source) }

// AddDay adds d to the row in one update, so rows added at once never lose
// one another's counts.
func (d *Days) AddDay(ctx context.Context, k usage.DayKey, v usage.Day, until time.Time) error {
	item := pkey(dayPK(k.Day), daySK(k))
	// Every name through a placeholder: several (agent among them) are DynamoDB's reserved words.
	names := map[string]string{"#ttl": "ttl", "#runs": "runs", "#turns": "turns", "#in": "input_tokens", "#out": "output_tokens",
		"#cr": "cache_read_tokens", "#cw": "cache_write_tokens", "#cw1": "cache_write_1h_tokens", "#cost": "cost_micro_usd",
		"#credits": "credits", "#provider": "provider", "#agent": "agent", "#src": "source"}
	_, err := d.db.UpdateItem(ctx, &ddb.UpdateItemInput{TableName: &d.table, Key: item,
		UpdateExpression: aws.String("ADD #runs :r, #turns :t, #in :i, #out :o, #cr :cr, #cw :cw, #cw1 :cw1, #cost :c, #credits :k " +
			"SET #ttl = :ttl, #provider = :p, #agent = :a, #src = :s"),
		ExpressionAttributeNames: names,
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":r": n(v.Runs), ":t": n(v.Turns), ":i": n(v.InputTokens), ":o": n(v.OutputTokens), ":cr": n(v.CacheRead),
			":cw": n(v.CacheWrite), ":cw1": n(v.CacheWrite1h), ":c": n(v.CostMicroUSD), ":k": n(v.Credits),
			":ttl": ttl(until), ":p": s(k.Provider), ":a": s(k.Agent), ":s": s(k.Source)}})
	return err
}

// DayRows reads a day's rows.
func (d *Days) DayRows(ctx context.Context, day string) ([]usage.DayRow, error) {
	var out []usage.DayRow
	var start map[string]types.AttributeValue
	for {
		res, err := d.db.Query(ctx, &ddb.QueryInput{TableName: &d.table, KeyConditionExpression: aws.String("pk = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(dayPK(day))}, ExclusiveStartKey: start})
		if err != nil {
			return nil, err
		}
		for _, it := range res.Items {
			out = append(out, usage.DayRow{
				DayKey: usage.DayKey{Day: day, Provider: getS(it, "provider"), Agent: getS(it, "agent"), Source: getS(it, "source")},
				Day: usage.Day{Runs: getN(it, "runs"), Turns: getN(it, "turns"), InputTokens: getN(it, "input_tokens"), OutputTokens: getN(it, "output_tokens"),
					CacheRead: getN(it, "cache_read_tokens"), CacheWrite: getN(it, "cache_write_tokens"), CacheWrite1h: getN(it, "cache_write_1h_tokens"),
					CostMicroUSD: getN(it, "cost_micro_usd"), Credits: getN(it, "credits")},
			})
		}
		if len(res.LastEvaluatedKey) == 0 {
			return out, nil
		}
		start = res.LastEvaluatedKey
	}
}
