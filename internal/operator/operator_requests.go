package operator

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"codexos/internal/store"
)

type operatorRequestRuntime interface {
	OperatorRequests() (store.OperatorRequestContext, error)
	OperatorRequest(uint64) (store.OperatorRequest, error)
	CreateOperatorRequest(string, string) (store.OperatorRequest, error)
	WithdrawOperatorRequest(uint64, string) (store.OperatorRequest, error)
	VerifyOperatorRequest(uint64, uint64, string) (store.OperatorRequest, error)
}

func (c *PlainConsole) executeOperatorRequest(line string, words []string) error {
	runtime, ok := c.runtime.(operatorRequestRuntime)
	if !ok {
		return errors.New("operator OS requests are unavailable")
	}
	if words[0] == "os-requests" && len(words) == 1 {
		context, err := runtime.OperatorRequests()
		if err != nil {
			return err
		}
		c.printLine("Operator OS requests (advisory; grant no capabilities):")
		for _, request := range context.Requests {
			c.printOperatorRequestView(request)
		}
		if len(context.Requests) == 0 {
			c.printLine("  none")
		}
		return nil
	}
	if words[0] != "os-request" || len(words) < 2 {
		return errors.New("usage: os-requests | os-request N | os-request create TITLE | DESCRIPTION | os-request withdraw N REASON | os-request verify N REPORT_REVISION NOTE")
	}
	var request store.OperatorRequest
	var err error
	switch words[1] {
	case "create":
		title, description, found := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "os-request")), "create")), "|")
		if !found {
			return errors.New("usage: os-request create TITLE | DESCRIPTION")
		}
		request, err = runtime.CreateOperatorRequest(strings.TrimSpace(title), strings.TrimSpace(description))
	case "withdraw", "verify":
		minimum := 4
		if words[1] == "verify" {
			minimum = 5
		}
		if len(words) < minimum {
			return errors.New("withdraw requires N REASON; verify requires N REPORT_REVISION NOTE")
		}
		id, parseErr := strconv.ParseUint(words[2], 10, 64)
		if parseErr != nil {
			return parseErr
		}
		if words[1] == "withdraw" {
			request, err = runtime.WithdrawOperatorRequest(id, strings.Join(words[3:], " "))
		} else {
			revision, parseErr := strconv.ParseUint(words[3], 10, 64)
			if parseErr != nil {
				return parseErr
			}
			request, err = runtime.VerifyOperatorRequest(id, revision, strings.Join(words[4:], " "))
		}
	default:
		if len(words) != 2 {
			return errors.New("usage: os-request N")
		}
		id, parseErr := strconv.ParseUint(words[1], 10, 64)
		if parseErr != nil {
			return parseErr
		}
		request, err = runtime.OperatorRequest(id)
		if err != nil {
			return err
		}
		c.printLine(fmt.Sprintf("Operator OS request #%d: %s", request.ID, EscapeTerminalText(request.Title, false)))
		c.printIndented(EscapeTerminalText(request.Description, true))
		context, err := runtime.OperatorRequests()
		if err != nil {
			return err
		}
		for _, view := range context.Requests {
			if view.ID == id {
				c.printOperatorRequestView(view)
			}
		}
		c.printLine("Attributed history (historical reports may be outside the selected lineage):")
		for _, entry := range request.History {
			generation := "none"
			if entry.Actor.Generation != nil {
				generation = strconv.FormatUint(*entry.Actor.Generation, 10)
			}
			c.printLine(EscapeTerminalText(fmt.Sprintf("  r%d %s %s %s by %s/%s run=%s gen=%s thread=%s turn=%s report=r%d", entry.Revision, entry.Timestamp, entry.Action, entry.Disposition, entry.Actor.Role, entry.Actor.Name, entry.Actor.RunID, generation, entry.Actor.ThreadID, entry.Actor.TurnID, entry.ReportRevision), false))
			if entry.Explanation != "" {
				c.printIndented(EscapeTerminalText(entry.Explanation, true))
			}
			if entry.Evidence != "" {
				c.printIndented("Completion evidence (implementor report): " + EscapeTerminalText(entry.Evidence, true))
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	c.recordOperator("os-request-"+words[1], "success", map[string]any{"request_id": request.ID, "revision": request.Revision()})
	c.printLine(fmt.Sprintf("Operator OS request #%d r%d recorded; active input is presented at the next supported implementor turn boundary.", request.ID, request.Revision()))
	return nil
}

func (c *PlainConsole) printOperatorRequestView(request store.OperatorRequestView) {
	state := "active"
	if !request.Active {
		state = "withdrawn"
	}
	report := "no applicable implementor report"
	if request.Report != nil {
		report = fmt.Sprintf("implementor-reported %s (report r%d)", request.Report.Disposition, request.Report.Revision)
		if request.Report.Disposition == "completed" {
			if request.Verification == nil {
				report += "; unverified"
			} else {
				report += fmt.Sprintf("; operator verified report r%d", request.Verification.ReportRevision)
			}
		}
	}
	c.printLine(fmt.Sprintf("  #%d r%d [%s] %s — %s", request.ID, request.Revision, state, EscapeTerminalText(request.Title, false), report))
}
