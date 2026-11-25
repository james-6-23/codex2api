package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/codex2api/database"
	"golang.org/x/net/html"
)

const (
	openAILatestPriceColumns = 5
	openAIOlderPriceColumns  = 4
)

// openAIPricingIslands 读取官网用于渲染折叠表格的数据，不执行网页脚本。
func openAIPricingIslands(body []byte, out map[string]database.ModelPricingOverride) error {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return err
	}
	var parseErr error
	walkPricingHTML(doc, func(node *html.Node) bool {
		if node.Data != "astro-island" || pricingNodeAttribute(node, "component-export") != "TextTokenPricingTables" {
			return true
		}
		var props map[string]json.RawMessage
		if err := json.Unmarshal([]byte(pricingNodeAttribute(node, "props")), &props); err != nil {
			parseErr = err
			return false
		}
		if err := openAIIslandRows(props, out); err != nil {
			parseErr = err
		}
		return false
	})
	return parseErr
}

func pricingNodeAttribute(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

// decodePricingProp 仅支持 Astro 的普通值和数组编码，未知编码直接拒绝。
func decodePricingProp(raw json.RawMessage) (any, error) {
	var pair []json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
		return nil, fmt.Errorf("OpenAI 页面数据编码不符")
	}
	var kind int
	if err := json.Unmarshal(pair[0], &kind); err != nil {
		return nil, err
	}
	if kind == 0 {
		var value any
		err := json.Unmarshal(pair[1], &value)
		return value, err
	}
	if kind != 1 {
		return nil, fmt.Errorf("OpenAI 页面数据编码类型不受支持")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(pair[1], &items); err != nil {
		return nil, err
	}
	values := make([]any, 0, len(items))
	for _, item := range items {
		value, err := decodePricingProp(item)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func openAIIslandRows(props map[string]json.RawMessage, out map[string]database.ModelPricingOverride) error {
	tier, err := decodePricingProp(props["tier"])
	if err != nil {
		return err
	}
	if tier != "standard" && tier != "fast" {
		return nil
	}
	decoded, err := decodePricingProp(props["rows"])
	if err != nil {
		return err
	}
	rows, ok := decoded.([]any)
	if !ok {
		return fmt.Errorf("OpenAI rows 格式不符")
	}
	table := officialHTMLPricingTable{tier: tier.(string)}
	for _, value := range rows {
		row, err := openAIIslandRow(value)
		if err != nil {
			return err
		}
		table.rows = append(table.rows, row)
	}
	return parseOpenAIPricingTable(table, out)
}

// openAIIslandModelLabel 读取模型列。旧版页面直接给字符串；新版把已下线模型
// 包成 {"label":[0,"gpt-5.4-nano"],"deprecated":[0,true]}，需要再解一层。
func openAIIslandModelLabel(value any) string {
	switch cell := value.(type) {
	case string:
		return strings.TrimSpace(cell)
	case map[string]any:
		label, ok := cell["label"]
		if !ok {
			return ""
		}
		encoded, err := json.Marshal(label)
		if err != nil {
			return ""
		}
		decoded, err := decodePricingProp(encoded)
		if err != nil {
			return ""
		}
		if text, ok := decoded.(string); ok {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func openAIIslandRow(value any) (officialPricingRow, error) {
	cells, ok := value.([]any)
	if !ok || (len(cells) != openAIOlderPriceColumns && len(cells) != openAILatestPriceColumns) {
		return nil, fmt.Errorf("OpenAI 文本价格数据列数不符")
	}
	// 模型列有两种编码：纯字符串，或 {"label":[0,"gpt-…"],"deprecated":[0,true]}。
	// 解析不出名字时留空，由 parseOpenAIPricingTable 跳过该行，避免单行格式变化
	// 中断整次官方价格同步。
	model := openAIIslandModelLabel(cells[0])
	if model == "" {
		return officialPricingRow{"model": ""}, nil
	}
	row := officialPricingRow{"model": model}
	for key, index := range map[string]int{"input": 1, "cached input": 2, "output": len(cells) - 1} {
		switch value := cells[index].(type) {
		case nil:
			if key != "cached input" {
				return nil, fmt.Errorf("OpenAI 必要价格数据缺失")
			}
			row[key] = "-"
		case float64:
			row[key] = "$" + strconv.FormatFloat(value, 'f', -1, 64)
		case string:
			if value != "-" {
				return nil, fmt.Errorf("OpenAI 价格数据不是数字")
			}
			row[key] = value
		default:
			return nil, fmt.Errorf("OpenAI 价格数据类型不符")
		}
	}
	return row, nil
}
