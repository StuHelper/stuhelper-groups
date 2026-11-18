package groups

import (
	"context"
	"errors"
	"slices"

	"github.com/StuHelper/stuhelper-platform/account"
)

type batchOutcome struct {
	Result
	SkipCode string `json:"skip_code,omitempty"`
}

func newBulkResult() BulkResult {
	return BulkResult{Items: []BulkItem{}, Executed: []BulkItem{}, Skipped: []BulkSkippedItem{}}
}

var batchReasons = map[string]string{
	"not_found":                "无成员记录",
	"not_joined":               "未入群",
	"removed":                  "成员已移出",
	"noop":                     "目标无需变更",
	"pending_profile":          "名片所需资料尚未就绪",
	"bot_disabled":             "本群未启用机器人",
	"action_queue_full":        "机器人操作队列已满",
	"card_check_disabled":      "本群未启用名片检查",
	"idempotency_mismatch":     "幂等键已用于其他请求",
	ErrInvalid.Error():         "操作参数无效",
	ErrUnprocessable.Error():   "操作内容不符合要求",
	ErrUnauthenticated.Error(): "需要登录",
	ErrForbidden.Error():       "缺少操作权限",
	ErrNotFound.Error():        "操作所需记录不存在",
	ErrConflict.Error():        "目标当前状态与操作冲突",
	ErrDisabled.Error():        "操作未启用",
	ErrCooldown.Error():        "操作受频率限制",
	ErrUnavailable.Error():     "暂时无法取得所需资料",
}

func (out *BulkResult) add(target string, outcome batchOutcome) {
	item := BulkItem{Target: target, Result: outcome.Result}
	out.Items = append(out.Items, item)
	code := outcome.SkipCode
	switch outcome.State {
	case "not_found", "not_joined", "noop", "pending_profile", "removed":
		code = outcome.State
	}
	if code == "" {
		out.Executed = append(out.Executed, item)
		return
	}
	out.Skipped = append(out.Skipped, BulkSkippedItem{Target: target, ReasonCode: code, Reason: batchReasons[code]})
}

// Only business rejections become terminal batch outcomes. A storage or provider
// failure rolls back the target and lets the caller resume it with the same key.
func batchRejection(cmd Command, err error) (batchOutcome, bool) {
	for _, base := range []error{ErrInvalid, ErrUnprocessable, ErrUnauthenticated, ErrForbidden, ErrNotFound, ErrConflict, ErrDisabled, ErrCooldown} {
		if errors.Is(err, base) {
			code := base.Error()
			var detail *ErrorDetail
			if errors.As(err, &detail) && batchReasons[detail.Reason] != "" {
				code = detail.Reason
			}
			return batchOutcome{Result: Result{Action: cmd.Kind, State: "rejected", Detail: base.Error()}, SkipCode: code}, true
		}
	}
	return batchOutcome{}, false
}

func (s *Service) batchConfig(ctx context.Context, tx ReadTx, p account.Principal, group, kind string) (GroupConfig, error) {
	var c GroupConfig
	if err := tx.Get("config", group, &c); err != nil {
		return c, err
	}
	if err := s.auth(ctx, p, "groups.manage", c); err != nil {
		return c, err
	}
	if cap := capability(kind); cap != "groups.manage" {
		return c, s.auth(ctx, p, cap, c)
	}
	return c, nil
}

func batchRequiresJoined(kind string) bool {
	return slices.Contains([]string{"warn", "mute", "unmute", "kick"}, kind)
}

// Return the prior member for preview formatting. The eligibility check and
// the command share the same transaction, including the write that queues a bot action.
func (s *Service) batchCommand(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command) (Member, Result, error) {
	var m Member
	err := tx.Get(bucket(c.ID, "members"), cmd.Target, &m)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return m, Result{}, err
	}
	if errors.Is(err, ErrNotFound) && cmd.Kind != "card" {
		return m, Result{Action: cmd.Kind, State: "not_found", Detail: "not_found"}, nil
	}
	if batchRequiresJoined(cmd.Kind) && (!m.Joined || m.State == "removed") {
		return m, Result{Action: cmd.Kind, State: "not_joined", Detail: m.State}, nil
	}
	result, err := s.command(ctx, tx, p, c, cmd, capability(cmd.Kind))
	return m, result, err
}

func (s *Service) executeBatchTarget(ctx context.Context, p account.Principal, group, key string, cmd Command) (batchOutcome, error) {
	input := struct {
		Group   string
		Command Command
	}{group, cmd}
	scopes := []string{"group:" + group, "receipt:" + p.Key() + ":" + key}
	const operation = "batch_target"
	id := hash([]string{p.Key(), operation, group, key})
	var out batchOutcome
	var rejected *batchOutcome
	attempt := func(tx *preparedTx) error {
		c, err := s.batchConfig(ctx, tx, p, group, cmd.Kind)
		if err != nil {
			return err
		}
		var saved batchOutcome
		if hit, err := replayReceipt(tx, id, input, &saved); hit || err != nil {
			out = saved
			return err
		}
		if rejected != nil {
			out = *rejected
			return s.saveReceipt(tx, p.Key(), operation, group, key, input, out)
		}

		// Earlier batches used the same child key and input as a single command.
		var result Result
		hit, err := replay(tx, p.Key(), "command", group, key, input, &result)
		if err == nil && !hit {
			_, result, err = s.batchCommand(ctx, tx, p, c, cmd)
		}
		if err != nil {
			if refusal, known := batchRejection(cmd, err); known {
				rejected = &refusal
			}
			return err
		}
		out = batchOutcome{Result: result}
		return s.saveReceipt(tx, p.Key(), operation, group, key, input, out)
	}
	err := s.runPrepared(ctx, scopes, attempt)
	if err != nil && rejected != nil {
		// A rejected target may already have staged writes. After rollback,
		// recheck authorization and any concurrent result before recording the rejection.
		err = s.runPrepared(ctx, scopes, attempt)
	}
	return out, err
}
