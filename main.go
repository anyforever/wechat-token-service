package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/go-resty/resty/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
	"gopkg.in/yaml.v3"
)

// -------------------------- 版本信息 --------------------------
var Version = "v1.0.0" // 编译时注入版本号

// -------------------------- 统一响应结构体 --------------------------
type Response struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data interface{} `json:"data"`
}

// -------------------------- 错误码常量 --------------------------
const (
	CodeSuccess      = 200
	CodeUnauthorized = 401
	CodeForbidden    = 403
	CodeParamError   = 422
	CodeServerError  = 500
)

// -------------------------- 配置结构体 --------------------------
type ServerConfig struct {
	Port         string        `yaml:"port"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	Prefix   string `yaml:"prefix"`
}

type WeChatConfig struct {
	TokenAPI            string        `yaml:"token_api"`
	RefreshAhead        int64         `yaml:"refresh_ahead"`
	AutoRefreshInterval time.Duration `yaml:"auto_refresh_interval"`
}

type APIClient struct {
	AppID  string `yaml:"appid"`
	Secret string `yaml:"secret"`
	Status string `yaml:"status"` // enabled/disabled
}

type APIConfig struct {
	RSAPublicKey    string      `yaml:"rsa_public_key"`
	SignExpireHours int         `yaml:"sign_expire_hours"`
	Clients         []APIClient `yaml:"clients"`
}

type LoggerConfig struct {
	Level      string `yaml:"level"`
	Encoding   string `yaml:"encoding"`
	OutputPath string `yaml:"output_path"`
	MaxSize    int    `yaml:"max_size"`
	MaxBackup  int    `yaml:"max_backup"`
	MaxAge     int    `yaml:"max_age"`
	Compress   bool   `yaml:"compress"`
}

type Account struct {
	AppID     string `yaml:"appid"`
	AppSecret string `yaml:"appsecret"`
}

type Config struct {
	Server   ServerConfig `yaml:"server"`
	Redis    RedisConfig  `yaml:"redis"`
	WeChat   WeChatConfig `yaml:"wechat"`
	API      APIConfig    `yaml:"api"`
	Logger   LoggerConfig `yaml:"logger"`
	Accounts []Account    `yaml:"accounts"`
}

// -------------------------- Token相关结构体 --------------------------
type TokenInfo struct {
	AccessToken string    `json:"access_token"`
	ExpiresIn   int64     `json:"expires_in"`
	ExpireTime  time.Time `json:"expire_time"`
	LastUpdate  time.Time `json:"last_update"`
}

// -------------------------- 全局变量 --------------------------
var (
	cfg          Config
	cfgMutex     sync.RWMutex
	logger       *zap.Logger
	redisClient  *redis.Client
	manager      *TokenManager
	ctx, ctxCancel = context.WithCancel(context.Background())
	rsaPublicKey *rsa.PublicKey
	clientCache  map[string]APIClient
	clientMutex  sync.RWMutex

	// 服务就绪状态
	serverReady bool
	readyMutex  sync.RWMutex

	// 配置热重载白名单
	hotReloadWhitelist = map[string]bool{
		"accounts":                     true,
		"api.clients":                  true,
		"api.sign_expire_hours":        true,
		"wechat.refresh_ahead":         true,
		"wechat.auto_refresh_interval": true,
		"wechat.token_api":             true,
	}
)

// -------------------------- 工具函数 - 统一响应 --------------------------
func Success(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, Response{
		Code: CodeSuccess,
		Msg:  "ok",
		Data: data,
	})
}

func Fail(c *gin.Context, code int, msg string) {
	httpCode := http.StatusOK
	switch code {
	case CodeUnauthorized, CodeForbidden:
		httpCode = http.StatusForbidden
	case CodeParamError:
		httpCode = http.StatusUnprocessableEntity
	case CodeServerError:
		httpCode = http.StatusInternalServerError
	}

	c.JSON(httpCode, Response{
		Code: code,
		Msg:  msg,
		Data: nil,
	})
}

// -------------------------- Token管理器 --------------------------
type TokenManager struct {
	accounts          map[string]Account
	mutex             sync.RWMutex
	client            *resty.Client
	weChatConfig      WeChatConfig
	configMutex       sync.RWMutex
	autoRefreshTicker *time.Ticker
	tickerMutex       sync.Mutex
	locker            *RedisLock
	isPreheating      bool
	Version           string // 添加版本字段，用于记录当前版本
}

func NewTokenManager(accounts []Account, weChatConfig WeChatConfig) *TokenManager {
	m := &TokenManager{
		accounts:     make(map[string]Account),
		client:       resty.New().SetTimeout(10 * time.Second),
		weChatConfig: weChatConfig,
		locker:       NewRedisLock(redisClient, ctx),
		Version:      Version, // 初始化版本
	}

	for _, acc := range accounts {
		m.accounts[acc.AppID] = acc
	}

	return m
}

// 更新公众号配置
func (m *TokenManager) UpdateAccounts(accounts []Account) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	newAccounts := make(map[string]Account)
	for _, acc := range accounts {
		newAccounts[acc.AppID] = acc
	}

	oldCount := len(m.accounts)
	newCount := len(newAccounts)
	m.accounts = newAccounts

	logger.Info("公众号配置已更新",
		zap.Int("old_account_count", oldCount),
		zap.Int("new_account_count", newCount))
}

// 更新微信配置
func (m *TokenManager) UpdateWeChatConfig(config WeChatConfig) {
	m.configMutex.Lock()
	defer m.configMutex.Unlock()

	oldConfig := m.weChatConfig
	m.weChatConfig.RefreshAhead = config.RefreshAhead
	m.weChatConfig.AutoRefreshInterval = config.AutoRefreshInterval
	m.weChatConfig.TokenAPI = config.TokenAPI

	m.restartAutoRefresh()

	logger.Info("微信配置已更新",
		zap.Int64("old_refresh_ahead", oldConfig.RefreshAhead),
		zap.Int64("new_refresh_ahead", config.RefreshAhead),
		zap.Duration("old_auto_refresh_interval", oldConfig.AutoRefreshInterval),
		zap.Duration("new_auto_refresh_interval", config.AutoRefreshInterval),
		zap.String("token_api", config.TokenAPI))
}

func (m *TokenManager) GetWeChatConfig() WeChatConfig {
	m.configMutex.RLock()
	defer m.configMutex.RUnlock()
	return m.weChatConfig
}

func (m *TokenManager) restartAutoRefresh() {
	m.tickerMutex.Lock()
	defer m.tickerMutex.Unlock()

	if m.autoRefreshTicker != nil {
		m.autoRefreshTicker.Stop()
	}

	go m.StartAutoRefresh()
}

// 预加载所有公众号Token
func (m *TokenManager) PreloadAllTokens() error {
	m.isPreheating = true
	defer func() { m.isPreheating = false }()

	logger.Info("开始预加载所有公众号Token（预热模式）")

	// 使用独立的预热前缀，不修改全局配置
	cfgMutex.RLock()
	preheatPrefix := fmt.Sprintf("%s_preheat", cfg.Redis.Prefix)
	cfgMutex.RUnlock()

	var wg sync.WaitGroup
	errChan := make(chan error, len(m.accounts))
	sem := make(chan struct{}, 10)

	m.mutex.RLock()
	appIDs := make([]string, 0, len(m.accounts))
	for appID := range m.accounts {
		appIDs = append(appIDs, appID)
	}
	m.mutex.RUnlock()

	for _, appID := range appIDs {
		wg.Add(1)
		sem <- struct{}{}

		go func(aid string) {
			defer wg.Done()
			defer func() { <-sem }()

			logger.Info("预加载Token", zap.String("appid", aid))
			_, err := m.getAccessTokenWithPrefix(aid, preheatPrefix)
			if err != nil {
				errMsg := fmt.Sprintf("预加载Token失败: appid=%s, err=%v", aid, err)
				logger.Error(errMsg)
				errChan <- errors.New(errMsg)
			} else {
				logger.Info("预加载Token成功", zap.String("appid", aid))
			}
		}(appID)
	}

	wg.Wait()
	close(errChan)

	var failedAccounts []string
	for err := range errChan {
		failedAccounts = append(failedAccounts, err.Error())
	}

	if len(failedAccounts) > 0 {
		// 降级处理：部分预热失败不阻止服务启动，自动刷新定时器会在下一轮补齐
		logger.Warn("部分公众号Token预热失败，服务将降级启动，自动刷新定时器将补齐缺失Token",
			zap.Strings("failed_accounts", failedAccounts),
			zap.Int("success_count", len(appIDs)-len(failedAccounts)),
			zap.Int("total_count", len(appIDs)))
	}

	logger.Info("所有公众号Token预加载完成", zap.Int("account_count", len(appIDs)))
	return nil
}

func (m *TokenManager) getTokenFromRedis(appid string) (*TokenInfo, error) {
	return m.getTokenFromRedisWithPrefix(appid, "")
}

func (m *TokenManager) getTokenFromRedisWithPrefix(appid, prefix string) (*TokenInfo, error) {
	if prefix == "" {
		cfgMutex.RLock()
		prefix = cfg.Redis.Prefix
		cfgMutex.RUnlock()
	}
	key := fmt.Sprintf("%s:token:%s", prefix, appid)
	data, err := redisClient.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		logger.Error("Redis获取token失败", zap.String("appid", appid), zap.Error(err))
		return nil, err
	}

	var tokenInfo TokenInfo
	if err := json.Unmarshal(data, &tokenInfo); err != nil {
		logger.Error("解析Redis token失败", zap.String("appid", appid), zap.Error(err))
		return nil, err
	}

	return &tokenInfo, nil
}

func (m *TokenManager) saveTokenToRedis(appid string, tokenInfo *TokenInfo) error {
	return m.saveTokenToRedisWithPrefix(appid, tokenInfo, "")
}

func (m *TokenManager) saveTokenToRedisWithPrefix(appid string, tokenInfo *TokenInfo, prefix string) error {
	if prefix == "" {
		cfgMutex.RLock()
		prefix = cfg.Redis.Prefix
		cfgMutex.RUnlock()
	}
	key := fmt.Sprintf("%s:token:%s", prefix, appid)
	data, err := json.Marshal(tokenInfo)
	if err != nil {
		logger.Error("序列化token失败", zap.String("appid", appid), zap.Error(err))
		return err
	}

	expire := time.Until(tokenInfo.ExpireTime) + 5*time.Minute
	if err := redisClient.SetEX(ctx, key, data, expire).Err(); err != nil {
		logger.Error("Redis保存token失败", zap.String("appid", appid), zap.Error(err))
		return err
	}

	return nil
}

func (m *TokenManager) isTokenValid(tokenInfo *TokenInfo) bool {
	if tokenInfo == nil || tokenInfo.AccessToken == "" {
		return false
	}

	weChatCfg := m.GetWeChatConfig()
	refreshTime := tokenInfo.ExpireTime.Add(-time.Second * time.Duration(weChatCfg.RefreshAhead))
	return time.Now().Before(refreshTime)
}

func (m *TokenManager) refreshTokenFromWeChat(appid string, account Account) (*TokenInfo, error) {
	weChatCfg := m.GetWeChatConfig()

	const maxRetries = 3
	retryDelay := 500 * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		tokenInfo, err := m.doRefreshTokenFromWeChat(appid, account, weChatCfg)
		if err == nil {
			return tokenInfo, nil
		}
		lastErr = err

		// 微信业务错误（如 appid/secret 错误）不重试，直接返回
		if isFatalWeChatError(err) {
			logger.Error("微信API返回不可重试的错误", zap.String("appid", appid), zap.Error(err))
			return nil, err
		}

		if attempt < maxRetries {
			logger.Warn("微信API请求失败，准备重试",
				zap.String("appid", appid),
				zap.Int("attempt", attempt),
				zap.Int("max_retries", maxRetries),
				zap.Duration("retry_after", retryDelay),
				zap.Error(err))
			time.Sleep(retryDelay)
			retryDelay *= 2 // 指数退避
		}
	}

	logger.Error("微信API请求重试耗尽", zap.String("appid", appid), zap.Int("retries", maxRetries), zap.Error(lastErr))
	return nil, fmt.Errorf("微信API请求失败（重试 %d 次后放弃）: %v", maxRetries, lastErr)
}

// isFatalWeChatError 判断是否为微信业务层不可重试错误（如 appid/secret 配置错误）
func isFatalWeChatError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// errcode 40001/40002/40013/40125 等表示 appid 或 secret 配置错误，重试无意义
	for _, code := range []string{"errcode=40001", "errcode=40002", "errcode=40013", "errcode=40125"} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

func (m *TokenManager) doRefreshTokenFromWeChat(appid string, account Account, weChatCfg WeChatConfig) (*TokenInfo, error) {
	resp, err := m.client.R().
		SetQueryParams(map[string]string{
			"grant_type": "client_credential",
			"appid":      account.AppID,
			"secret":     account.AppSecret,
		}).
		Get(weChatCfg.TokenAPI)

	if err != nil {
		logger.Error("请求微信API失败", zap.String("appid", appid), zap.Error(err))
		return nil, fmt.Errorf("请求微信API失败: %v", err)
	}

	if resp.StatusCode() != http.StatusOK {
		logger.Error("微信API返回非200", zap.String("appid", appid), zap.Int("status_code", resp.StatusCode()), zap.String("body", resp.String()))
		return nil, fmt.Errorf("微信API返回错误: status=%d, body=%s", resp.StatusCode(), resp.String())
	}

	var wxResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Errcode     int    `json:"errcode"`
		Errmsg      string `json:"errmsg"`
	}

	if err := json.Unmarshal(resp.Body(), &wxResp); err != nil {
		logger.Error("解析微信API响应失败", zap.String("appid", appid), zap.Error(err), zap.String("body", resp.String()))
		return nil, fmt.Errorf("解析微信响应失败: %v", err)
	}

	if wxResp.Errcode != 0 {
		logger.Error("微信API返回错误码", zap.String("appid", appid), zap.Int("errcode", wxResp.Errcode), zap.String("errmsg", wxResp.Errmsg))
		return nil, fmt.Errorf("微信API错误: errcode=%d, errmsg=%s", wxResp.Errcode, wxResp.Errmsg)
	}

	now := time.Now()
	return &TokenInfo{
		AccessToken: wxResp.AccessToken,
		ExpiresIn:   wxResp.ExpiresIn,
		ExpireTime:  now.Add(time.Second * time.Duration(wxResp.ExpiresIn)),
		LastUpdate:  now,
	}, nil
}

func (m *TokenManager) GetAccessToken(appid string) (*TokenInfo, error) {
	return m.getAccessTokenWithPrefix(appid, "")
}

// getAccessTokenWithPrefix 支持指定 Redis 前缀，供预热等场景隔离存储使用
func (m *TokenManager) getAccessTokenWithPrefix(appid, prefix string) (*TokenInfo, error) {
	m.mutex.RLock()
	account, exists := m.accounts[appid]
	m.mutex.RUnlock()

	if !exists {
		logger.Warn("公众号配置不存在", zap.String("appid", appid))
		return nil, fmt.Errorf("公众号配置不存在: %s", appid)
	}

	// 第一步：从Redis读取Token
	tokenInfo, err := m.getTokenFromRedisWithPrefix(appid, prefix)
	if err != nil {
		logger.Warn("从Redis获取Token失败", zap.String("appid", appid), zap.Error(err))
	}

	// 第二步：检查Token是否有效，有效则直接返回
	if m.isTokenValid(tokenInfo) {
		return tokenInfo, nil
	}

	// 第三步：Token失效，申请分布式锁准备刷新
	lockExpire := 30 * time.Second
	lockSuccess, lockToken, err := m.locker.Lock(appid, lockExpire)
	if err != nil {
		return nil, fmt.Errorf("申请分布式锁失败: %v", err)
	}

	// 未拿到锁：等待1秒后重新读取Redis（此时可能已被其他节点刷新）
	if !lockSuccess {
		logger.Debug("未拿到分布式锁，等待后重试读取Token", zap.String("appid", appid))
		time.Sleep(1 * time.Second)
		tokenInfo, err = m.getTokenFromRedisWithPrefix(appid, prefix)
		if err != nil {
			return nil, fmt.Errorf("等待锁释放后读取Token失败: %v", err)
		}
		if !m.isTokenValid(tokenInfo) {
			return nil, fmt.Errorf("等待锁释放后Token仍然无效（appid=%s），请稍后重试", appid)
		}
		return tokenInfo, nil
	}

	// 拿到锁：确保解锁（即使panic也能释放）
	defer func() {
		if err := m.locker.Unlock(appid, lockToken); err != nil {
			logger.Error("解锁失败", zap.String("appid", appid), zap.Error(err))
		}
	}()

	// 再次检查Redis（防止等待期间已被其他节点刷新）
	tokenInfo, err = m.getTokenFromRedisWithPrefix(appid, prefix)
	if err == nil && m.isTokenValid(tokenInfo) {
		return tokenInfo, nil
	}

	// 最终确认Token失效，调用微信API刷新
	newToken, err := m.refreshTokenFromWeChat(appid, account)
	if err != nil {
		return nil, err
	}

	// 统一保存到当前使用的 prefix（默认正式 key，预热时为 preheat key），避免双写
	if saveErr := m.saveTokenToRedisWithPrefix(appid, newToken, prefix); saveErr != nil {
		logger.Warn("保存Token到Redis失败", zap.String("appid", appid), zap.Error(saveErr))
	} else {
		logger.Info("刷新Token成功", zap.String("appid", appid), zap.Time("expire_time", newToken.ExpireTime))
	}

	return newToken, nil
}

// 自动刷新逻辑：仅主节点执行刷新
func (m *TokenManager) StartAutoRefresh() {
	m.tickerMutex.Lock()
	weChatCfg := m.GetWeChatConfig()
	m.autoRefreshTicker = time.NewTicker(weChatCfg.AutoRefreshInterval)
	ticker := m.autoRefreshTicker
	m.tickerMutex.Unlock()

	logger.Info("自动刷新定时器已启动", zap.Duration("interval", weChatCfg.AutoRefreshInterval))

	go func() {
		defer ticker.Stop()
		for range ticker.C {
			isMaster, err := m.electMaster()
			if err != nil {
				logger.Error("主节点选举失败", zap.Error(err))
				continue
			}
			if !isMaster {
				logger.Debug("非主节点，跳过自动刷新")
				continue
			}

			logger.Info("主节点开始自动刷新所有公众号Token")

			m.mutex.RLock()
			appIDs := make([]string, 0, len(m.accounts))
			for appID := range m.accounts {
				appIDs = append(appIDs, appID)
			}
			m.mutex.RUnlock()

			var wg sync.WaitGroup
			sem := make(chan struct{}, 10)

			for _, appID := range appIDs {
				wg.Add(1)
				sem <- struct{}{}

				go func(aid string) {
					defer wg.Done()
					defer func() { <-sem }()

					_, err := m.GetAccessToken(aid)
					if err != nil {
						logger.Error("自动刷新Token失败", zap.String("appid", aid), zap.Error(err))
					}
				}(appID)
			}

			wg.Wait()

			logger.Info("主节点自动刷新完成", zap.Int("total_accounts", len(appIDs)))
		}
	}()
}

// 主节点选举：锁的有效期仅覆盖一个刷新周期，宕机后下一周期其他节点可自动接管
func (m *TokenManager) electMaster() (bool, error) {
	masterKey := fmt.Sprintf("%s:lock:master", cfg.Redis.Prefix)
	// 锁有效期 = 刷新间隔 + 30秒缓冲，仅覆盖当前轮次，避免主节点宕机后长时间无法切换
	masterExpire := m.GetWeChatConfig().AutoRefreshInterval + 30*time.Second
	res, err := redisClient.SetNX(ctx, masterKey, "1", masterExpire).Result()
	if err != nil {
		return false, err
	}
	return res, nil
}

// -------------------------- Redis分布式锁工具 --------------------------

// unlockScript: 原子性验证 token 后删除锁，防止误删其他节点持有的锁
var unlockScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("del", KEYS[1])
else
    return 0
end
`)

type RedisLock struct {
	client *redis.Client
	ctx    context.Context
}

func NewRedisLock(client *redis.Client, ctx context.Context) *RedisLock {
	return &RedisLock{
		client: client,
		ctx:    ctx,
	}
}

// genLockToken 生成唯一锁标识，用于防止误删其他节点的锁
func genLockToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// fallback: 时间戳+随机后缀
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().Unix())
	}
	return fmt.Sprintf("%x", b)
}

func (l *RedisLock) Lock(appid string, expire time.Duration) (bool, string, error) {
	lockKey := fmt.Sprintf("%s:lock:token:%s", cfg.Redis.Prefix, appid)
	token := genLockToken()
	res, err := l.client.SetNX(l.ctx, lockKey, token, expire).Result()
	if err != nil {
		logger.Error("Redis加锁失败", zap.String("appid", appid), zap.Error(err))
		return false, "", err
	}
	return res, token, nil
}

func (l *RedisLock) Unlock(appid, token string) error {
	lockKey := fmt.Sprintf("%s:lock:token:%s", cfg.Redis.Prefix, appid)
	result, err := unlockScript.Run(l.ctx, l.client, []string{lockKey}, token).Int()
	if err != nil {
		logger.Warn("Redis解锁失败", zap.String("appid", appid), zap.Error(err))
		return err
	}
	if result == 0 {
		logger.Warn("Redis解锁被拒绝（token不匹配，锁已被其他节点持有或已过期）",
			zap.String("appid", appid))
	}
	return nil
}

// -------------------------- 签名验证工具函数 --------------------------
func loadRSAPublicKey() error {
	cfgMutex.RLock()
	publicKeyContent := cfg.API.RSAPublicKey
	cfgMutex.RUnlock()

	block, _ := pem.Decode([]byte(publicKeyContent))
	if block == nil {
		return errors.New("解析公钥失败")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("解析公钥内容失败: %v", err)
	}

	var ok bool
	rsaPublicKey, ok = pub.(*rsa.PublicKey)
	if !ok {
		return errors.New("公钥类型不是RSA")
	}

	logger.Info("RSA公钥加载成功")
	return nil
}

func initClientCache() {
	clientMutex.Lock()
	defer clientMutex.Unlock()

	clientCache = make(map[string]APIClient)
	cfgMutex.RLock()
	for _, client := range cfg.API.Clients {
		clientCache[client.AppID] = client
	}
	cfgMutex.RUnlock()

	logger.Info("客户端缓存初始化完成", zap.Int("client_count", len(clientCache)))
}

func updateClientCache(clients []APIClient) {
	clientMutex.Lock()
	defer clientMutex.Unlock()

	newClientCache := make(map[string]APIClient)
	for _, client := range clients {
		newClientCache[client.AppID] = client
	}

	oldCount := len(clientCache)
	newCount := len(newClientCache)
	clientCache = newClientCache

	logger.Info("客户端缓存已更新",
		zap.Int("old_client_count", oldCount),
		zap.Int("new_client_count", newCount))
}

func getClient(appid string) (APIClient, bool) {
	clientMutex.RLock()
	defer clientMutex.RUnlock()
	client, exists := clientCache[appid]
	return client, exists
}

func verifySignature(appid, signature, timestampStr string) (int, error) {
	client, exists := getClient(appid)
	if !exists {
		return CodeUnauthorized, fmt.Errorf("客户端不存在: %s", appid)
	}

	if client.Status != "enabled" {
		return CodeForbidden, fmt.Errorf("客户端已禁用: %s", appid)
	}

	timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return CodeParamError, fmt.Errorf("时间戳格式错误: %v", err)
	}

	cfgMutex.RLock()
	signExpireHours := cfg.API.SignExpireHours
	cfgMutex.RUnlock()

	signTime := time.Unix(timestamp, 0)
	now := time.Now()
	if now.Sub(signTime) > time.Duration(signExpireHours)*time.Hour {
		return CodeUnauthorized, fmt.Errorf("签名已过期，签名时间: %s，当前时间: %s，有效期: %d小时",
			signTime.Format(time.RFC3339), now.Format(time.RFC3339), signExpireHours)
	}

	signStr := fmt.Sprintf("%s|%s|%s", appid, client.Secret, timestampStr)
	signBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return CodeParamError, fmt.Errorf("签名Base64解码失败: %v", err)
	}

	hash := sha256.New()
	hash.Write([]byte(signStr))
	if err := rsa.VerifyPKCS1v15(rsaPublicKey, crypto.SHA256, hash.Sum(nil), signBytes); err != nil {
		return CodeUnauthorized, fmt.Errorf("签名验证失败: %v", err)
	}

	return CodeSuccess, nil
}

// -------------------------- 中间件 --------------------------
func readinessMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		readyMutex.RLock()
		ready := serverReady
		readyMutex.RUnlock()

		if !ready {
			Fail(c, CodeServerError, "服务正在预热中，请稍后重试")
			c.Abort()
			return
		}

		c.Next()
	}
}

func rsaAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		appid := c.GetHeader("X-AppId")
		signature := c.GetHeader("X-Signature")
		timestamp := c.GetHeader("X-Timestamp")

		if appid == "" || signature == "" || timestamp == "" {
			logger.Warn("鉴权参数缺失",
				zap.String("ip", c.ClientIP()),
				zap.String("appid", appid),
				zap.Bool("has_signature", signature != ""),
				zap.Bool("has_timestamp", timestamp != ""))

			Fail(c, CodeParamError, "缺少鉴权参数（X-AppId/X-Signature/X-Timestamp）")
			c.Abort()
			return
		}

		code, err := verifySignature(appid, signature, timestamp)
		if err != nil {
			logger.Warn("签名验证失败",
				zap.String("ip", c.ClientIP()),
				zap.String("appid", appid),
				zap.Error(err))

			Fail(c, code, err.Error())
			c.Abort()
			return
		}

		c.Set("client_appid", appid)
		logger.Debug("鉴权成功", zap.String("ip", c.ClientIP()), zap.String("appid", appid))

		c.Next()
	}
}

// -------------------------- HTTP处理器 --------------------------
func healthHandler(c *gin.Context) {
	readyMutex.RLock()
	ready := serverReady
	readyMutex.RUnlock()

	cfgMutex.RLock()
	accountCount := len(cfg.Accounts)
	clientCount := len(cfg.API.Clients)
	cfgMutex.RUnlock()

	healthData := gin.H{
		"status":        "healthy",
		"ready":         ready,
		"account_count": accountCount,
		"client_count":  clientCount,
		"hot_reload":    "enabled",
		"timestamp":     time.Now().Format(time.RFC3339),
		"version":       Version,
	}

	Success(c, healthData)
}

func getTokenHandler(c *gin.Context) {
	wechatAppid := c.Query("appid")
	if wechatAppid == "" {
		Fail(c, CodeParamError, "公众号appid不能为空")
		return
	}

	tokenInfo, err := manager.GetAccessToken(wechatAppid)
	if err != nil {
		logger.Error("获取Token失败", zap.String("appid", wechatAppid), zap.Error(err))
		Fail(c, CodeServerError, fmt.Sprintf("获取Token失败: %s", err.Error()))
		return
	}

	responseData := gin.H{
		"appid":         wechatAppid,
		"access_token":  tokenInfo.AccessToken,
		"expires_in":    tokenInfo.ExpiresIn,
		"expire_time":   tokenInfo.ExpireTime.Format(time.RFC3339),
		"last_update":   tokenInfo.LastUpdate.Format(time.RFC3339),
		"refresh_ahead": manager.GetWeChatConfig().RefreshAhead,
	}

	Success(c, responseData)
}

// -------------------------- 配置热重载核心逻辑 --------------------------
func diffConfig(oldCfg, newCfg *Config) map[string]interface{} {
	diff := make(map[string]interface{})

	// 检查公众号配置（同时比对 AppID 和 AppSecret，防止改密钥不触发热重载）
	if len(oldCfg.Accounts) != len(newCfg.Accounts) {
		diff["accounts"] = newCfg.Accounts
	} else {
		// key = appid, value = appsecret
		accountMap := make(map[string]string)
		for _, acc := range oldCfg.Accounts {
			accountMap[acc.AppID] = acc.AppSecret
		}
		for _, acc := range newCfg.Accounts {
			oldSecret, exists := accountMap[acc.AppID]
			if !exists || oldSecret != acc.AppSecret {
				diff["accounts"] = newCfg.Accounts
				break
			}
		}
	}

	// 检查客户端配置（同时比对 AppID、Secret 和 Status）
	if len(oldCfg.API.Clients) != len(newCfg.API.Clients) {
		diff["api.clients"] = newCfg.API.Clients
	} else {
		type clientSnapshot struct {
			secret string
			status string
		}
		clientMap := make(map[string]clientSnapshot)
		for _, cli := range oldCfg.API.Clients {
			clientMap[cli.AppID] = clientSnapshot{secret: cli.Secret, status: cli.Status}
		}
		for _, cli := range newCfg.API.Clients {
			old, exists := clientMap[cli.AppID]
			if !exists || old.secret != cli.Secret || old.status != cli.Status {
				diff["api.clients"] = newCfg.API.Clients
				break
			}
		}
	}

	// 检查签名过期时间
	if oldCfg.API.SignExpireHours != newCfg.API.SignExpireHours {
		diff["api.sign_expire_hours"] = newCfg.API.SignExpireHours
	}

	// 检查微信配置
	if oldCfg.WeChat.RefreshAhead != newCfg.WeChat.RefreshAhead {
		diff["wechat.refresh_ahead"] = newCfg.WeChat.RefreshAhead
	}
	if oldCfg.WeChat.AutoRefreshInterval != newCfg.WeChat.AutoRefreshInterval {
		diff["wechat.auto_refresh_interval"] = newCfg.WeChat.AutoRefreshInterval
	}
	if oldCfg.WeChat.TokenAPI != newCfg.WeChat.TokenAPI {
		diff["wechat.token_api"] = newCfg.WeChat.TokenAPI
	}

	return diff
}

func isChangeInWhitelist(changeKey string) bool {
	return hotReloadWhitelist[changeKey]
}

func validateConfig(c *Config) error {
	for _, acc := range c.Accounts {
		if acc.AppID == "" || acc.AppSecret == "" {
			return fmt.Errorf("公众号配置异常: appid=%s 缺少appsecret", acc.AppID)
		}
	}

	for _, cli := range c.API.Clients {
		if cli.Status != "enabled" && cli.Status != "disabled" {
			return fmt.Errorf("客户端配置异常: appid=%s status非法", cli.AppID)
		}
		if cli.AppID == "" || cli.Secret == "" {
			return fmt.Errorf("客户端配置异常: appid=%s 缺少secret", cli.AppID)
		}
	}

	if c.WeChat.RefreshAhead < 0 {
		return fmt.Errorf("refresh_ahead不能为负数")
	}
	if c.WeChat.AutoRefreshInterval <= 0 {
		return fmt.Errorf("auto_refresh_interval必须大于0")
	}
	if c.API.SignExpireHours <= 0 {
		return fmt.Errorf("sign_expire_hours必须大于0")
	}

	return nil
}

func reloadConfig() error {
	file, err := os.Open("config/config.yaml")
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %v", err)
	}
	defer file.Close()

	var newCfg Config
	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&newCfg); err != nil {
		return fmt.Errorf("解析配置文件失败: %v", err)
	}

	if err := validateConfig(&newCfg); err != nil {
		return fmt.Errorf("配置校验失败: %v", err)
	}

	cfgMutex.RLock()
	oldCfg := cfg
	cfgMutex.RUnlock()

	changes := diffConfig(&oldCfg, &newCfg)
	if len(changes) == 0 {
		logger.Info("配置无变更，无需热重载")
		return nil
	}

	allowedChanges := make(map[string]interface{})
	var deniedChanges []string

	for key, value := range changes {
		if isChangeInWhitelist(key) {
			allowedChanges[key] = value
		} else {
			deniedChanges = append(deniedChanges, key)
		}
	}

	if len(deniedChanges) > 0 {
		logger.Warn("配置变更项不在热重载白名单内，已忽略", zap.Strings("denied_changes", deniedChanges))
	}

	if len(allowedChanges) == 0 {
		return nil
	}

	cfgMutex.Lock()
	// 保留基础配置（非白名单项）
	newCfg.Server = oldCfg.Server
	newCfg.Redis = oldCfg.Redis
	newCfg.Logger = oldCfg.Logger
	newCfg.API.RSAPublicKey = oldCfg.API.RSAPublicKey

	cfg = newCfg
	cfgMutex.Unlock()

	updateClientCache(cfg.API.Clients)
	manager.UpdateAccounts(cfg.Accounts)
	manager.UpdateWeChatConfig(cfg.WeChat)

	logger.Info("配置热重载成功",
		zap.Any("allowed_changes", allowedChanges),
		zap.Strings("denied_changes", deniedChanges),
		zap.String("update_time", time.Now().Format(time.RFC3339)))

	return nil
}

func startConfigHotReload() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Fatal("配置热重载监听初始化失败", zap.Error(err))
	}
	defer watcher.Close()

	if err := watcher.Add("config/config.yaml"); err != nil {
		logger.Fatal("添加配置文件监听失败", zap.Error(err))
	}

	logger.Info("配置热重载已启动", zap.String("watch_file", "config/config.yaml"))

	const debounceDuration = 500 * time.Millisecond
	var (
		debounceTimer *time.Timer
		debounceMu    sync.Mutex
	)

	// triggerReload 重置防抖定时器，避免频繁写入时多次触发
	triggerReload := func(filename string) {
		debounceMu.Lock()
		defer debounceMu.Unlock()

		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		debounceTimer = time.AfterFunc(debounceDuration, func() {
			logger.Info("检测到配置文件变更，开始热重载", zap.String("file", filename))
			if err := reloadConfig(); err != nil {
				logger.Error("配置热重载失败", zap.Error(err))
			}
		})
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				logger.Warn("配置监听事件通道已关闭")
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				triggerReload(event.Name)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				logger.Warn("配置监听错误通道已关闭")
				return
			}
			logger.Warn("配置文件监听错误", zap.Error(err))
		}
	}
}

// -------------------------- 工具函数 --------------------------
func initLogger() {
	cfgMutex.RLock()
	loggerCfg := cfg.Logger
	cfgMutex.RUnlock()

	level := zapcore.InfoLevel
	switch loggerCfg.Level {
	case "debug":
		level = zapcore.DebugLevel
	case "warn":
		level = zapcore.WarnLevel
	case "error":
		level = zapcore.ErrorLevel
	}

	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "timestamp",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	var writeSyncer zapcore.WriteSyncer
	if loggerCfg.OutputPath == "stdout" {
		writeSyncer = zapcore.AddSync(os.Stdout)
	} else {
		lumberjackLogger := &lumberjack.Logger{
			Filename:   loggerCfg.OutputPath,
			MaxSize:    loggerCfg.MaxSize,
			MaxBackups: loggerCfg.MaxBackup,
			MaxAge:     loggerCfg.MaxAge,
			Compress:   loggerCfg.Compress,
		}
		writeSyncer = zapcore.AddSync(lumberjackLogger)
	}

	var encoder zapcore.Encoder
	if loggerCfg.Encoding == "console" {
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	} else {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	}

	core := zapcore.NewCore(encoder, writeSyncer, level)
	logger = zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1))
	zap.ReplaceGlobals(logger)
}

func loadConfig() {
	file, err := os.Open("config/config.yaml")
	if err != nil {
		panic(fmt.Sprintf("加载配置文件失败: %v", err))
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&cfg); err != nil {
		panic(fmt.Sprintf("解析配置文件失败: %v", err))
	}

	if err := validateConfig(&cfg); err != nil {
		panic(fmt.Sprintf("初始配置校验失败: %v", err))
	}
}

func initRedis() {
	// 添加超时控制，防止长时间阻塞
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	redisClient = redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})

	if err := redisClient.Ping(ctx).Err(); err != nil {
		panic(fmt.Sprintf("Redis连接失败: %v", err))
	}
	logger.Info("Redis连接成功", zap.String("addr", cfg.Redis.Addr))
}

func setServerReady(ready bool) {
	readyMutex.Lock()
	defer readyMutex.Unlock()
	serverReady = ready
	logger.Info("服务就绪状态已更新", zap.Bool("ready", ready))
}

// -------------------------- 主函数 --------------------------
func main() {
	// 1. 加载配置
	loadConfig()

	// 2. 初始化日志
	initLogger()
	defer logger.Sync()

	// 3. 初始化Redis
	initRedis()
	defer redisClient.Close()

	// 4. 加载RSA公钥
	if err := loadRSAPublicKey(); err != nil {
		logger.Fatal("加载RSA公钥失败", zap.Error(err))
	}

	// 5. 初始化客户端缓存
	initClientCache()

	// 6. 初始化Token管理器
	manager = NewTokenManager(cfg.Accounts, cfg.WeChat)
	logger.Info("Token管理器初始化完成", zap.Int("account_count", len(cfg.Accounts)))

	// 7. 启动配置热重载
	go startConfigHotReload()

	// 8. 启动前预热：预加载所有公众号Token（失败降级处理，不阻止启动）
	logger.Info("开始服务预热...")
	setServerReady(false)
	if err := manager.PreloadAllTokens(); err != nil {
		// PreloadAllTokens 已内部降级，此处仅作兜底日志
		logger.Warn("Token预加载出现异常，服务将降级启动", zap.Error(err))
	}

	// 9. 预热完成，标记服务就绪
	setServerReady(true)
	logger.Info("服务预热完成，已就绪")

	// 10. 启动自动刷新
	manager.StartAutoRefresh()

	// 11. 初始化Gin引擎
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	r.Use(gin.Recovery())
	r.Use(readinessMiddleware())

	// 公开路由：不需要鉴权（用于负载均衡/K8s 探针等）
	open := r.Group("/api/v1")
	{
		open.GET("/health", healthHandler)
	}

	// 受保护路由：需要 RSA 签名鉴权
	v1 := r.Group("/api/v1")
	v1.Use(rsaAuthMiddleware())
	{
		v1.GET("/wechat/token", getTokenHandler)
	}

	// 12. 启动HTTP服务
	srv := &http.Server{
		Addr:         cfg.Server.Port,
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("HTTP服务启动失败", zap.Error(err))
		}
	}()

	logger.Info("微信公众号Token管理系统启动成功",
		zap.String("port", cfg.Server.Port),
		zap.String("version", Version),
		zap.Bool("hot_reload_enabled", true))

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("开始关闭服务...")

	// 取消全局 ctx，使所有 Redis 操作（含 RedisLock）感知关闭信号
	ctxCancel()

	manager.tickerMutex.Lock()
	if manager.autoRefreshTicker != nil {
		manager.autoRefreshTicker.Stop()
	}
	manager.tickerMutex.Unlock()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Fatal("服务关闭失败", zap.Error(err))
	}

	logger.Info("服务已优雅关闭")
}
