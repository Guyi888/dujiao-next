package upstream

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/logger"
	"github.com/dujiao-next/internal/models"
	"github.com/google/uuid"
)

// CaihongAdapter 彩虹发卡协议适配器
//
// 认证方式：
//
//	AppId:        appID（用户名）
//	AppToken:     sha1(appID + appSecret + path + timestamp)
//	AppTimestamp: Unix 时间戳（秒）
type CaihongAdapter struct {
	baseURL    string
	appID      string // 对应彩虹的 username / AppId
	appSecret  string // 对应彩虹的 password / AppSecret
	uploadsDir string
	client     *http.Client
}

// NewCaihongAdapter 创建彩虹发卡适配器
func NewCaihongAdapter(conn *models.SiteConnection, uploadsDir string) *CaihongAdapter {
	return &CaihongAdapter{
		baseURL:    strings.TrimRight(conn.BaseURL, "/"),
		appID:      conn.ApiKey,
		appSecret:  conn.ApiSecret,
		uploadsDir: uploadsDir,
		client:     &http.Client{Timeout: 30 * time.Second},
	}
}

// ========== 彩虹 API 响应结构 ==========

type caihongBaseResp struct {
	Code   int             `json:"code"`
	Msg    string          `json:"msg"`
	Result json.RawMessage `json:"result"`
}

type caihongGoodsListResult struct {
	Data []caihongGoodsItem `json:"data"`
}

type caihongGoodsItem struct {
	GoodsSN    string `json:"goodsSN"`
	GoodsName  string `json:"goodsName"`
	GoodsThumb string `json:"goodsThumb"`
}

type caihongGoodsDetail struct {
	GoodsSN        string                 `json:"goodsSN"`
	GoodsName      string                 `json:"goodsName"`
	GoodsThumb     string                 `json:"goodsThumb"`
	GoodsDetail    string                 `json:"goodsDetail"`
	GoodsPrice     float64                `json:"goodsPrice"`
	MinOrderNum    int                    `json:"minOrderNum"`
	MaxOrderNum    int                    `json:"maxOrderNum"`
	IsClose        bool                   `json:"isClose"`
	GoodsType      int                    `json:"goodsType"`
	CategoryID     string                 `json:"categoryId"`
	CategoryName   string                 `json:"categoryName"`
	ParamsTemplate []caihongParamTemplate `json:"paramsTemplate"`
}

type caihongParamTemplate struct {
	Alias string `json:"alias"`
	Name  string `json:"name"`
}

type caihongCategoryResult struct {
	Data []caihongCategoryItem `json:"data"`
}

type caihongCategoryItem struct {
	CategoryID   string `json:"categoryId"`
	CategoryName string `json:"categoryName"`
}

type caihongCreateOrderResp struct {
	OrderSN string `json:"orderSN"`
}

type caihongOrderDetail struct {
	OrderSN     string `json:"orderSN"`
	OrderState  int    `json:"orderState"`
	CardNumber  string `json:"cardNumber"`
	StartNum    int    `json:"startNum"`
	CurrentNum  int    `json:"currentNum"`
	FinishTotal int    `json:"finishTotal"`
}

// ========== 工具函数 ==========

// goodsSNtoID 将彩虹 goodsSN（字符串）确定性映射为 uint ID
// 使用 SHA-256 前 8 字节，碰撞概率极低
func goodsSNtoID(sn string) uint {
	h := sha256.Sum256([]byte(sn))
	id := binary.BigEndian.Uint64(h[:8])
	return uint(id >> 1) // 右移保证正值且不超 int64
}

// caihongSign 生成彩虹签名
// sha1(appID + appSecret + path + timestamp)
func caihongSign(appID, appSecret, path string, timestamp int64) string {
	raw := fmt.Sprintf("%s%s%s%d", appID, appSecret, path, timestamp)
	h := sha1.New()
	h.Write([]byte(raw))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// caihongOrderStatus 将彩虹订单状态码映射为独角Next采购单状态
// 彩虹状态：0=待处理 1=已完成 2=处理中 3=待确认 4=已失败 5=退款中 6=已退款 7=退款可 8=处理完
func caihongOrderStatus(state int) string {
	switch state {
	case 1, 8:
		return constants.ProcurementStatusFulfilled
	case 2, 3:
		return constants.ProcurementStatusAccepted
	case 4:
		return constants.ProcurementStatusFailed
	case 5, 6, 7:
		return constants.ProcurementStatusRefunded
	default:
		return constants.ProcurementStatusPending
	}
}

// ========== Adapter 接口实现 ==========

// Ping 连接测试：调用商品列表接口验证连通性
func (a *CaihongAdapter) Ping(ctx context.Context) (*PingResult, error) {
	path := "/api/client/goods/v2/goods/list"
	if _, err := a.doRequest(ctx, "GET", path, nil); err != nil {
		return nil, fmt.Errorf("caihong ping: %w", err)
	}
	return &PingResult{
		SiteName:        a.baseURL,
		ProtocolVersion: "caihong-v2",
		Currency:        "CNY",
	}, nil
}

// ListCategories 拉取上游分类列表
// 部分彩虹站点无分类接口，降级返回空列表
func (a *CaihongAdapter) ListCategories(ctx context.Context) (*CategoryListResult, error) {
	path := "/api/client/goods/v2/category"
	resp, err := a.doRequest(ctx, "GET", path, nil)
	if err != nil {
		logger.Warnw("caihong_list_categories_failed", "err", err)
		return &CategoryListResult{Supported: false, Categories: []UpstreamCategory{}}, nil
	}

	var result caihongCategoryResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return &CategoryListResult{Supported: false, Categories: []UpstreamCategory{}}, nil
	}

	categories := make([]UpstreamCategory, 0, len(result.Data))
	for i, c := range result.Data {
		categories = append(categories, UpstreamCategory{
			ID:        uint(i + 1),
			ParentID:  0,
			Slug:      c.CategoryID,
			Name:      models.JSON{"zh-CN": c.CategoryName},
			SortOrder: i,
		})
	}
	return &CategoryListResult{Supported: true, Categories: categories}, nil
}

// ListProducts 拉取上游商品列表
// 彩虹 API 一次返回全量，本方法在本地做分页裁剪
func (a *CaihongAdapter) ListProducts(ctx context.Context, opts ListProductsOpts) (*ProductListResult, error) {
	path := "/api/client/goods/v2/goods/list"
	resp, err := a.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("caihong list products: %w", err)
	}

	var listResult caihongGoodsListResult
	if err := json.Unmarshal(resp, &listResult); err != nil {
		return nil, fmt.Errorf("caihong list products: parse: %w", err)
	}

	all := listResult.Data
	total := len(all)

	// 分页裁剪
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	page := opts.Page
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * pageSize
	if start >= total {
		return &ProductListResult{Total: total, Items: []UpstreamProduct{}}, nil
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	slice := all[start:end]

	items := make([]UpstreamProduct, 0, len(slice))
	for _, g := range slice {
		thumb := g.GoodsThumb
		if thumb != "" && strings.HasPrefix(thumb, "/") {
			thumb = a.baseURL + thumb
		}
		images := []string{}
		if thumb != "" {
			images = []string{thumb}
		}

		// SKUCode 存储 goodsSN，供 CreateOrder 使用
		skus := []UpstreamSKU{
			{
				ID:            goodsSNtoID(g.GoodsSN),
				SKUCode:       g.GoodsSN, // 关键：goodsSN 存这里
				PriceAmount:   "0",
				StockStatus:   constants.ProductStockStatusUnlimited,
				StockQuantity: -1,
				IsActive:      true,
			},
		}

		items = append(items, UpstreamProduct{
			ID:              goodsSNtoID(g.GoodsSN),
			Title:           models.JSON{"zh-CN": g.GoodsName},
			Images:          images,
			IsActive:        true,
			FulfillmentType: constants.FulfillmentTypeUpstream,
			SKUs:            skus,
			UpdatedAt:       time.Now(),
		})
	}

	return &ProductListResult{Total: total, Items: items}, nil
}

// GetProduct 获取单个商品详情
// 彩虹用字符串 goodsSN 查询，独角Next 用 uint ID，
// 本实现通过调用 ListProducts 找到对应商品后再拉取详情
func (a *CaihongAdapter) GetProduct(ctx context.Context, productID uint) (*UpstreamProduct, error) {
	// 先拉全量列表找到对应的 goodsSN
	allResult, err := a.ListProducts(ctx, ListProductsOpts{Page: 1, PageSize: 10000})
	if err != nil {
		return nil, fmt.Errorf("caihong get product: list products: %w", err)
	}

	var goodsSN string
	for _, p := range allResult.Items {
		if p.ID == productID {
			if len(p.SKUs) > 0 {
				goodsSN = p.SKUs[0].SKUCode
			}
			break
		}
	}
	if goodsSN == "" {
		return nil, fmt.Errorf("caihong get product: productID %d not found", productID)
	}

	// 拉取商品详情
	path := "/api/client/goods/v2/goods?goodsSN=" + goodsSN
	resp, err := a.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("caihong get product detail: %w", err)
	}

	var detail caihongGoodsDetail
	if err := json.Unmarshal(resp, &detail); err != nil {
		return nil, fmt.Errorf("caihong get product detail: parse: %w", err)
	}

	thumb := detail.GoodsThumb
	if thumb != "" && strings.HasPrefix(thumb, "/") {
		thumb = a.baseURL + thumb
	}
	images := []string{}
	if thumb != "" {
		images = []string{thumb}
	}

	stockStatus := constants.ProductStockStatusUnlimited
	if detail.IsClose {
		stockStatus = constants.ProductStockStatusOutOfStock
	}

	price := fmt.Sprintf("%.2f", detail.GoodsPrice)
	skus := []UpstreamSKU{
		{
			ID:            goodsSNtoID(detail.GoodsSN),
			SKUCode:       detail.GoodsSN,
			PriceAmount:   price,
			StockStatus:   stockStatus,
			StockQuantity: -1,
			IsActive:      !detail.IsClose,
		},
	}

	return &UpstreamProduct{
		ID:              goodsSNtoID(detail.GoodsSN),
		Title:           models.JSON{"zh-CN": detail.GoodsName},
		Description:     models.JSON{"zh-CN": detail.GoodsDetail},
		Content:         models.JSON{"zh-CN": detail.GoodsDetail},
		Images:          images,
		PriceAmount:     price,
		IsActive:        !detail.IsClose,
		FulfillmentType: constants.FulfillmentTypeUpstream,
		SKUs:            skus,
		UpdatedAt:       time.Now(),
	}, nil
}

// CreateOrder 发起采购单
// goodsSN 从 req.ManualFormData["goods_sn"] 或 SKUCode 获取
func (a *CaihongAdapter) CreateOrder(ctx context.Context, req CreateUpstreamOrderReq) (*CreateUpstreamOrderResp, error) {
	// 优先从 ManualFormData 取 goodsSN
	goodsSN := ""
	if req.ManualFormData != nil {
		if v, ok := req.ManualFormData["goods_sn"]; ok {
			goodsSN = fmt.Sprintf("%v", v)
		}
	}
	if goodsSN == "" {
		return &CreateUpstreamOrderResp{
			OK:           false,
			ErrorMessage: "missing goods_sn in ManualFormData",
		}, nil
	}

	// 构造彩虹下单请求参数
	params := []map[string]string{}
	if req.ManualFormData != nil {
		for k, v := range req.ManualFormData {
			if k == "goods_sn" {
				continue
			}
			params = append(params, map[string]string{
				"alias": k,
				"value": fmt.Sprintf("%v", v),
			})
		}
	}

	body := map[string]interface{}{
		"goodsSN":   goodsSN,
		"buyNotify": -1,
		"number":    req.Quantity,
		"params":    params,
	}

	path := "/api/client/goods/v2/order"
	resp, err := a.doRequest(ctx, "POST", path, body)
	if err != nil {
		return &CreateUpstreamOrderResp{OK: false, ErrorMessage: err.Error()}, nil
	}

	var orderResp caihongCreateOrderResp
	if err := json.Unmarshal(resp, &orderResp); err != nil {
		return &CreateUpstreamOrderResp{
			OK:           false,
			ErrorMessage: "parse order response: " + err.Error(),
		}, nil
	}

	return &CreateUpstreamOrderResp{
		OK:      true,
		OrderNo: orderResp.OrderSN,
		Status:  constants.ProcurementStatusSubmitted,
	}, nil
}

// GetOrder 查询上游订单状态
// 彩虹侧用 orderSN（字符串）查询，独角Next 传 uint orderID
// 实际 orderSN 存储在 ProcurementOrder.UpstreamOrderNo 中，
// 此处通过 OrderNo 字段传入（调用方负责设置）
func (a *CaihongAdapter) GetOrder(ctx context.Context, orderID uint) (*UpstreamOrderDetail, error) {
	// 独角Next 标准接口以 uint 查询，彩虹需要 orderSN 字符串
	// 框架会先查本地 ProcurementOrder 得到 OrderNo，再调用此处
	// 所以此方法返回 not supported，实际由 GetOrderByNo 处理
	return nil, fmt.Errorf("caihong: GetOrder by numeric ID not supported; framework should use OrderNo")
}

// GetOrderByNo 通过 orderSN 字符串查询彩虹订单（供服务层直接调用）
func (a *CaihongAdapter) GetOrderByNo(ctx context.Context, orderSN string) (*UpstreamOrderDetail, error) {
	path := "/api/client/goods/v2/order?orderSN=" + orderSN
	resp, err := a.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("caihong get order: %w", err)
	}

	var detail caihongOrderDetail
	if err := json.Unmarshal(resp, &detail); err != nil {
		return nil, fmt.Errorf("caihong get order: parse: %w", err)
	}

	status := caihongOrderStatus(detail.OrderState)

	var fulfillment *UpstreamFulfillment
	if detail.CardNumber != "" {
		now := time.Now()
		fulfillment = &UpstreamFulfillment{
			Type:        constants.FulfillmentTypeUpstream,
			Status:      constants.FulfillmentStatusDelivered,
			Payload:     detail.CardNumber,
			DeliveredAt: &now,
		}
	}

	return &UpstreamOrderDetail{
		OrderNo:     detail.OrderSN,
		Status:      status,
		Fulfillment: fulfillment,
	}, nil
}

// CancelOrder 彩虹上游不支持取消订单
func (a *CaihongAdapter) CancelOrder(ctx context.Context, orderID uint) error {
	return fmt.Errorf("caihong upstream does not support order cancellation")
}

// DownloadImage 下载上游图片到本地存储
func (a *CaihongAdapter) DownloadImage(ctx context.Context, imageURL string) (string, error) {
	fullURL := imageURL
	if strings.HasPrefix(imageURL, "/") {
		fullURL = a.baseURL + imageURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return "", fmt.Errorf("create download request: %w", err)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download image: status %d", resp.StatusCode)
	}

	ext := filepath.Ext(imageURL)
	if ext == "" || len(ext) > 6 {
		ext = ".jpg"
	}
	if idx := strings.Index(ext, "?"); idx > 0 {
		ext = ext[:idx]
	}

	filename := uuid.New().String() + ext
	dir := filepath.Join(a.uploadsDir, "upstream")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create uploads dir: %w", err)
	}

	filePath := filepath.Join(dir, filename)
	f, err := os.Create(filePath)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	return "/uploads/upstream/" + filename, nil
}

// ========== 内部 HTTP 请求方法 ==========

// doRequest 发送带彩虹签名的 HTTP 请求，返回 result 字段的原始 JSON
func (a *CaihongAdapter) doRequest(ctx context.Context, method, path string, body interface{}) (json.RawMessage, error) {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
	}

	timestamp := time.Now().Unix()
	token := caihongSign(a.appID, a.appSecret, path, timestamp)

	url := a.baseURL + path
	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = strings.NewReader(string(bodyBytes))
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("AppId", a.appID)
	req.Header.Set("AppToken", token)
	req.Header.Set("AppTimestamp", fmt.Sprintf("%d", timestamp))
	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		logger.Warnw("caihong_request_error",
			"method", method, "path", path,
			"status", resp.StatusCode, "body", string(respBody))
		return nil, fmt.Errorf("caihong responded with status %d: %s", resp.StatusCode, string(respBody))
	}

	var base caihongBaseResp
	if err := json.Unmarshal(respBody, &base); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if base.Code != 100 {
		return nil, fmt.Errorf("caihong error %d: %s", base.Code, base.Msg)
	}

	return base.Result, nil
}
