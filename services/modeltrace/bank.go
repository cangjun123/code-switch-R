// Package modeltrace 实现基于数字分布指纹的模型归因检测。
// 算法与指纹库移植自 ModelTrace (MIT License, https://github.com/xqy2006/ModelTrace)。
package modeltrace

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed unified_bank.json
var unifiedBankJSON []byte

const (
	ValueMin = 1
	ValueMax = 355
	// Dimension 数字值域维度
	Dimension = ValueMax - ValueMin + 1
	// Alpha 直方图平滑系数
	Alpha = 0.5
	// MinimumValidNumbers 单条回答可接受的最少有效数字数
	MinimumValidNumbers = 80
)

// BankModel 指纹库中的单个模型条目
type BankModel struct {
	ID                string `json:"id"`
	DisplayName       string `json:"display_name"`
	Family            string `json:"family"`
	FamilyName        string `json:"family_name"`
	ResponseCount     int    `json:"response_count"`
	ValidNumberCount  int    `json:"valid_number_count"`
	Counts            []int  `json:"counts"`
}

// BankCalibration 单一查询数量对应的 softmax 校准参数
type BankCalibration struct {
	Beta       float64 `json:"beta"`
	CVAccuracy float64 `json:"cv_accuracy"`
}

// BankRobustPart robust 评分所需的统计量（Hellinger 与有序块各一份）
type BankRobustPart struct {
	FeatureMean         []float64    `json:"feature_mean"`
	FeatureScale        []float64    `json:"feature_scale"`
	NuisanceBasis       [][]float64  `json:"nuisance_basis"`
	Centroids           [][]float64  `json:"centroids"`
	EnvironmentCentroids [][][]float64 `json:"environment_centroids,omitempty"`
	Weight              float64      `json:"weight,omitempty"`
}

// bankFile unified_bank.json 的原始结构
type bankFile struct {
	RecommendedQueries int                          `json:"recommended_queries"`
	Models             []BankModel                  `json:"models"`
	Robust             struct {
		ModelOrder     []string        `json:"model_order"`
		Hellinger      BankRobustPart  `json:"hellinger"`
		OrderedBlocks  BankRobustPart  `json:"ordered_blocks"`
	} `json:"robust"`
	Calibration map[string]BankCalibration `json:"calibration"`
}

// Bank 解析后的指纹库
type Bank struct {
	Models      []BankModel
	ModelOrder  []string
	Hellinger   BankRobustPart
	Ordered     BankRobustPart
	Calibration map[string]BankCalibration

	modelIndex map[string]int
}

var (
	bankOnce sync.Once
	bankInst *Bank
	bankErr  error
)

// LoadBank 解析内置的统一指纹库（进程内单例）
func LoadBank() (*Bank, error) {
	bankOnce.Do(func() {
		var file bankFile
		if err := json.Unmarshal(unifiedBankJSON, &file); err != nil {
			bankErr = fmt.Errorf("解析指纹库失败: %w", err)
			return
		}
		if len(file.Models) == 0 {
			bankErr = fmt.Errorf("指纹库为空")
			return
		}
		bankInst = &Bank{
			Models:      file.Models,
			ModelOrder:  file.Robust.ModelOrder,
			Hellinger:   file.Robust.Hellinger,
			Ordered:     file.Robust.OrderedBlocks,
			Calibration: file.Calibration,
			modelIndex:  make(map[string]int, len(file.Models)),
		}
		for i, model := range file.Models {
			bankInst.modelIndex[model.ID] = i
		}
	})
	return bankInst, bankErr
}

// ModelIDs 返回指纹库覆盖的模型 ID 列表（保持库内顺序）
func (b *Bank) ModelIDs() []string {
	ids := make([]string, len(b.Models))
	for i, model := range b.Models {
		ids[i] = model.ID
	}
	return ids
}

// ContainsModel 判断模型是否在指纹库覆盖范围内
func (b *Bank) ContainsModel(modelID string) bool {
	_, ok := b.modelIndex[modelID]
	return ok
}

// SupportedModels 返回指纹库覆盖的模型描述（供前端选择器使用）
type ModelOption struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Family      string `json:"family"`
	FamilyName  string `json:"familyName"`
}

func (b *Bank) SupportedModels() []ModelOption {
	options := make([]ModelOption, 0, len(b.Models))
	for _, model := range b.Models {
		options = append(options, ModelOption{
			ID:          model.ID,
			DisplayName: model.DisplayName,
			Family:      model.Family,
			FamilyName:  model.FamilyName,
		})
	}
	return options
}
