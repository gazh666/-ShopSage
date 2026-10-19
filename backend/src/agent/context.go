package agent

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"shopsage/backend/src/domain"
)

// buildUserPreferences 加载并合并用户长期偏好记忆。它以“增量覆盖”的方式把本轮用户话术中
// 新提取到的肤质/预算信号写回数据库，实现跨会话持久化；提取失败或无需更新时不产生副作用。
func (r *Runtime) buildUserPreferences(ctx context.Context, run domain.AgentRun, query string) domain.UserPreferences {
	prefs := domain.UserPreferences{AccountID: run.AccountID}
	if strings.TrimSpace(run.AccountID) == "" || !r.boolConfig(ctx, "agent.preferences.enabled", true) {
		return prefs
	}
	if existing, ok := r.store.GetUserPreferences(ctx, run.AccountID); ok {
		prefs = existing
	}
	if prefs.AccountID == "" {
		prefs.AccountID = run.AccountID
	}
	detected := extractPreferences(query)
	changed := false
	if detected.SkinType != "" && detected.SkinType != prefs.SkinType {
		prefs.SkinType = detected.SkinType
		changed = true
	}
	if detected.BudgetMin != "" && detected.BudgetMin != prefs.BudgetMin {
		prefs.BudgetMin = detected.BudgetMin
		changed = true
	}
	if detected.BudgetMax != "" && detected.BudgetMax != prefs.BudgetMax {
		prefs.BudgetMax = detected.BudgetMax
		changed = true
	}
	if changed {
		if err := r.store.UpsertUserPreferences(ctx, prefs); err != nil {
			r.logger.Warn("upsert user preferences failed", "account_id", run.AccountID, "error", err)
		} else if strings.TrimSpace(run.RunID) != "" {
			r.trace(ctx, run, "memory", "preferences_updated", "", "ok", 0, "", map[string]any{
				"skin_type":  prefs.SkinType,
				"budget_min": prefs.BudgetMin,
				"budget_max": prefs.BudgetMax,
			})
		}
	}
	return prefs
}

// buildLayeredContext 把上下文按 4 类独立存储拆开，只把压缩后的相关部分拼装进 prompt：
//   1. 当前任务状态（购物车）；
//   2. 短期对话上下文（最近几轮相关记忆）；
//   3. 用户长期记忆（肤质/预算等偏好，来自数据库）；
//   4. 外部检索信息（RAG 商品/工具返回，仅在 ReAct 循环中以 observation 形式追加，不进初始 prompt）。
// 每一类都受独立字符预算约束，超出部分截断，从源头控制 prompt 长度与 token 开销。
func (r *Runtime) buildLayeredContext(ctx context.Context, run domain.AgentRun, query string, memory conversationMemory, prefs domain.UserPreferences) string {
	maxChars := r.intConfig(ctx, "context.max_chars", 1600)
	shortTermChars := r.intConfig(ctx, "context.short_term_chars", 700)
	longTermChars := r.intConfig(ctx, "context.long_term_chars", 400)
	if maxChars <= 0 {
		maxChars = 1600
	}
	if shortTermChars <= 0 {
		shortTermChars = 700
	}
	if longTermChars <= 0 {
		longTermChars = 400
	}

	var b strings.Builder
	b.WriteString("当前用户问题：\n")
	b.WriteString(truncateRunes(strings.TrimSpace(query), maxChars/2))
	b.WriteString("\n")

	// 1. 当前任务状态：购物车仅在非空时注入，避免为无购物车的会话浪费 token。
	if cart := r.store.GetCart(ctx, run.AccountID); len(cart.Items) > 0 {
		if text := truncateRunes(cartSummaryText(cart), 240); text != "" {
			b.WriteString("\n【当前任务状态 - 购物车】\n")
			b.WriteString(text)
			b.WriteString("\n")
		}
	}

	// 2. 短期对话上下文：复用 memory 检索结果，只保留本轮真正相关的记录与商品指代。
	if memory.HasRelevantMemory {
		if text := truncateRunes(shortTermContextText(memory), shortTermChars); text != "" {
			b.WriteString("\n【短期对话上下文】\n")
			b.WriteString(text)
			b.WriteString("\n")
		}
	}

	// 3. 用户长期记忆：偏好注入，受独立预算约束。
	if text := truncateRunes(preferencesContextText(prefs), longTermChars); text != "" {
		b.WriteString("\n【用户长期记忆】\n")
		b.WriteString(text)
		b.WriteString("\n")
	}

	b.WriteString("\n上下文使用规则：以上分层信息只用于补充回答与约束判断；检索类信息会在工具调用阶段以 observation 形式提供。最终回答直接回应当前问题，不要复述这些规则。")
	return truncateRunes(b.String(), maxChars)
}

func shortTermContextText(memory conversationMemory) string {
	var b strings.Builder
	if strings.TrimSpace(memory.MemorySummary) != "" {
		b.WriteString("- ")
		b.WriteString(strings.TrimSpace(memory.MemorySummary))
		b.WriteString("\n")
	}
	if len(memory.ReferencedProductIDs) > 0 {
		b.WriteString("- 相关历史商品：")
		indexed := indexedMemoryProductsByID(memory.Records, memory.ReferencedProductIDs)
		written := map[string]bool{}
		for _, product := range indexed {
			written[product.ProductID] = true
			b.WriteString(fmt.Sprintf(" 第%d轮第%d个 item_id=%s sku_id=%s name=%s price=%s；", product.TurnIndex, product.ProductIndex, product.ProductID, product.SkuID, product.Name, product.Price))
		}
		for _, item := range missingIndexedProductIDs(memory.Records, memory.ReferencedProductIDs, written) {
			b.WriteString(fmt.Sprintf(" 第%d轮第%d个 item_id=%s；", item.TurnIndex, item.ProductIndex, item.ProductID))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func preferencesContextText(prefs domain.UserPreferences) string {
	if prefs.AccountID == "" && prefs.SkinType == "" && prefs.BudgetMin == "" && prefs.BudgetMax == "" && len(prefs.Categories) == 0 && len(prefs.Brands) == 0 {
		return ""
	}
	var b strings.Builder
	parts := make([]string, 0, 4)
	if strings.TrimSpace(prefs.SkinType) != "" {
		parts = append(parts, "肤质 "+prefs.SkinType)
	}
	if strings.TrimSpace(prefs.BudgetMin) != "" || strings.TrimSpace(prefs.BudgetMax) != "" {
		budget := "预算"
		if strings.TrimSpace(prefs.BudgetMin) != "" {
			budget += " " + prefs.BudgetMin + " 及以上"
		}
		if strings.TrimSpace(prefs.BudgetMax) != "" {
			budget += " " + prefs.BudgetMax + " 以内"
		}
		parts = append(parts, budget)
	}
	if len(prefs.Categories) > 0 {
		parts = append(parts, "常用品类 "+strings.Join(prefs.Categories, "/"))
	}
	if len(prefs.Brands) > 0 {
		parts = append(parts, "偏好品牌 "+strings.Join(prefs.Brands, "/"))
	}
	if len(parts) == 0 {
		return ""
	}
	b.WriteString(strings.Join(parts, "；"))
	return b.String()
}

func cartSummaryText(cart domain.Cart) string {
	var b strings.Builder
	for i, item := range cart.Items {
		if i > 0 {
			b.WriteString("；")
		}
		b.WriteString(fmt.Sprintf("第%d件 %s x%d(price=%s,item_id=%s,sku_id=%s)", i+1, item.Name, item.Quantity, item.Price, item.ProductID, item.SkuID))
	}
	return b.String()
}

var (
	skinTypePattern = regexp.MustCompile(`油皮|油性皮肤|干皮|干性皮肤|混合性皮肤|混合皮|敏感肌|敏感性皮肤|痘痘肌|中性皮肤`)
	budgetMaxRe     = regexp.MustCompile(`(?:预算|不超过|以内|以下|最高)\s*[:：]?\s*(\d{1,6})`)
	budgetMinRe     = regexp.MustCompile(`(?:预算|至少|以上|最低)\s*[:：]?\s*(\d{1,6})`)
	priceRangeRe    = regexp.MustCompile(`(\d{1,6})\s*[-~到至]\s*(\d{1,6})`)
)

// extractPreferences 用轻量规则从用户话术中提取稳定的肤质和预算信号，作为长期记忆的增量来源。
// 这里只做“置信度高”的精确提取，复杂意图仍由 Planner/LLM 负责，不额外增加大模型调用。
func extractPreferences(query string) domain.UserPreferences {
	prefs := domain.UserPreferences{}
	if skin := skinTypePattern.FindString(query); skin != "" {
		prefs.SkinType = normalizeSkinType(skin)
	}
	if match := priceRangeRe.FindStringSubmatch(query); len(match) == 3 {
		if min, err := strconv.Atoi(match[1]); err == nil {
			prefs.BudgetMin = strconv.Itoa(min)
		}
		if max, err := strconv.Atoi(match[2]); err == nil {
			prefs.BudgetMax = strconv.Itoa(max)
		}
		return prefs
	}
	if match := budgetMaxRe.FindStringSubmatch(query); len(match) == 2 {
		prefs.BudgetMax = match[1]
	}
	if match := budgetMinRe.FindStringSubmatch(query); len(match) == 2 {
		prefs.BudgetMin = match[1]
	}
	return prefs
}

func normalizeSkinType(skin string) string {
	switch skin {
	case "油性皮肤", "油皮":
		return "油皮"
	case "干性皮肤", "干皮":
		return "干皮"
	case "混合性皮肤", "混合皮":
		return "混合皮"
	case "敏感性皮肤", "敏感肌":
		return "敏感肌"
	case "痘痘肌":
		return "痘痘肌"
	case "中性皮肤":
		return "中性皮肤"
	default:
		return skin
	}
}