package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// multica issue record {add|list} — append-only decision and experiment-run
// ledger on an issue. See docs/design/issue-record.md.

var issueRecordCmd = &cobra.Command{
	Use:   "record",
	Short: "Append and list an issue's decision and run records",
}

var issueRecordAddCmd = &cobra.Command{
	Use:   "add <issue-id>",
	Short: "Append a decision or run record",
	Long: `Append one immutable record. Records cannot be edited or deleted: to correct
one, or to conclude a pending run, append a new record with --supersedes <seq>.

Verdicts: decision = pending|keep|revert; run = pending|pass|fail|inconclusive.
The request UUID is printed; retry an uncertain write with --request-id <uuid>.`,
	Args: exactArgs(1),
	RunE: runIssueRecordAdd,
}

var issueRecordListCmd = &cobra.Command{
	Use:   "list <issue-id>",
	Short: "List an issue's records in order",
	Args:  exactArgs(1),
	RunE:  runIssueRecordList,
}

func init() {
	issueRecordCmd.AddCommand(issueRecordAddCmd)
	issueRecordCmd.AddCommand(issueRecordListCmd)

	f := issueRecordAddCmd.Flags()
	f.String("kind", "", "Record kind: decision or run (required)")
	f.String("summary", "", "One-line summary (required)")
	f.String("why", "", "Rationale")
	f.StringArray("evidence", nil, "Evidence pointer: URL, commit, job ID or path (repeatable)")
	f.String("verdict", "", "Verdict (required)")
	f.StringArray("field", nil, "Structured field as key=value, value typed like metadata set (repeatable)")
	f.Int("supersedes", 0, "Seq of an earlier record of the same kind this one corrects or concludes")
	f.String("request-id", "", "Request UUID for an idempotent retry (default: generated)")
	f.String("output", "table", "Output format: table or json")

	issueRecordListCmd.Flags().String("kind", "", "Filter by kind: decision or run")
	issueRecordListCmd.Flags().String("output", "table", "Output format: table, json or tsv")

	issueCmd.AddCommand(issueRecordCmd)
}

type issueRecord struct {
	Seq           int            `json:"seq"`
	ClientRequest string         `json:"client_request_id"`
	Kind          string         `json:"kind"`
	Summary       string         `json:"summary"`
	Rationale     string         `json:"rationale"`
	Evidence      []string       `json:"evidence"`
	Verdict       string         `json:"verdict"`
	Fields        map[string]any `json:"fields"`
	SupersedesSeq *int           `json:"supersedes_seq"`
	AuthorType    string         `json:"author_type"`
	AuthorID      string         `json:"author_id"`
	CreatedAt     string         `json:"created_at"`
}

func buildIssueRecordFields(pairs []string) (map[string]json.RawMessage, error) {
	fields := make(map[string]json.RawMessage, len(pairs))
	for _, pair := range pairs {
		key, raw, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--field %q must be in key=value form", pair)
		}
		if _, dup := fields[key]; dup {
			return nil, fmt.Errorf("--field %q given more than once", key)
		}
		value, err := parseMetadataValue(raw, "")
		if err != nil {
			return nil, fmt.Errorf("--field %s: %w", key, err)
		}
		fields[key] = value
	}
	return fields, nil
}

func runIssueRecordAdd(cmd *cobra.Command, args []string) error {
	flags := cmd.Flags()
	kind, _ := flags.GetString("kind")
	summary, _ := flags.GetString("summary")
	verdict, _ := flags.GetString("verdict")
	if kind == "" || summary == "" || verdict == "" {
		return fmt.Errorf("--kind, --summary and --verdict are required")
	}
	why, _ := flags.GetString("why")
	evidence, _ := flags.GetStringArray("evidence")
	pairs, _ := flags.GetStringArray("field")
	fields, err := buildIssueRecordFields(pairs)
	if err != nil {
		return err
	}
	requestID, _ := flags.GetString("request-id")
	if requestID == "" {
		requestID = uuid.NewString()
	}
	body := map[string]any{
		"client_request_id": requestID,
		"kind":              kind,
		"summary":           summary,
		"rationale":         why,
		"evidence":          evidence,
		"verdict":           verdict,
		"fields":            fields,
	}
	if flags.Changed("supersedes") {
		seq, _ := flags.GetInt("supersedes")
		body["supersedes_seq"] = seq
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issueRef, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	var rec issueRecord
	if err := client.PostJSON(ctx, "/api/issues/"+issueRef.ID+"/records", body, &rec); err != nil {
		return fmt.Errorf("add record (retry with --request-id %s): %w", requestID, err)
	}
	output, _ := flags.GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, rec)
	}
	printIssueRecordTable(os.Stdout, []issueRecord{rec})
	return nil
}

func runIssueRecordList(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issueRef, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	path := "/api/issues/" + issueRef.ID + "/records"
	if kind, _ := cmd.Flags().GetString("kind"); kind != "" {
		path += "?kind=" + url.QueryEscape(kind)
	}
	var result struct {
		Records []issueRecord `json:"records"`
	}
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return fmt.Errorf("list records: %w", err)
	}
	switch output, _ := cmd.Flags().GetString("output"); output {
	case "json":
		return cli.PrintJSON(os.Stdout, result.Records)
	case "tsv":
		writeIssueRecordTSV(os.Stdout, result.Records)
	default:
		printIssueRecordTable(os.Stdout, result.Records)
	}
	return nil
}

func formatIssueRecordFields(fields map[string]any) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+formatMetadataValue(fields[k]))
	}
	return strings.Join(parts, " ")
}

func issueRecordSupersedes(rec issueRecord) string {
	if rec.SupersedesSeq == nil {
		return ""
	}
	return strconv.Itoa(*rec.SupersedesSeq)
}

func printIssueRecordTable(w io.Writer, records []issueRecord) {
	headers := []string{"SEQ", "KIND", "VERDICT", "SUPERSEDES", "SUMMARY", "EVIDENCE"}
	rows := make([][]string, 0, len(records))
	for _, rec := range records {
		rows = append(rows, []string{
			strconv.Itoa(rec.Seq), rec.Kind, rec.Verdict, issueRecordSupersedes(rec),
			rec.Summary, strings.Join(rec.Evidence, " "),
		})
	}
	cli.PrintTable(w, headers, rows)
}

// tsvCell keeps every record on one line so the output stays a valid table.
func tsvCell(s string) string {
	return strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(s)
}

func writeIssueRecordTSV(w io.Writer, records []issueRecord) {
	fmt.Fprintln(w, "seq\tkind\tverdict\tsupersedes\tsummary\trationale\tevidence\tfields\tauthor_type\tcreated_at")
	for _, rec := range records {
		cells := []string{
			strconv.Itoa(rec.Seq), rec.Kind, rec.Verdict, issueRecordSupersedes(rec),
			rec.Summary, rec.Rationale, strings.Join(rec.Evidence, " "),
			formatIssueRecordFields(rec.Fields), rec.AuthorType, rec.CreatedAt,
		}
		for i := range cells {
			cells[i] = tsvCell(cells[i])
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
}
