package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestNormalizeConfigPathReturnsAbsolutePath(t *testing.T) {
	configPath, err := normalizeConfigPath("config/config.yaml")
	if err != nil {
		t.Fatalf("normalize config path: %v", err)
	}
	if !filepath.IsAbs(configPath) {
		t.Fatalf("expected absolute config path, got %q", configPath)
	}
}

func TestResolveConfigAssetPathUsesConfigDirectory(t *testing.T) {
	baseDir := filepath.Join("/tmp", "deploy", "conf")
	resolved := resolveConfigAssetPath(baseDir, "keys/public.pem")
	expected := filepath.Clean(filepath.Join(baseDir, "keys/public.pem"))
	if resolved != expected {
		t.Fatalf("expected %q, got %q", expected, resolved)
	}
}

func TestResolveConfigAssetPathStripsConfigDirPrefix(t *testing.T) {
	baseDir := filepath.Join("/tmp", "deploy", "conf")
	resolved := resolveConfigAssetPath(baseDir, "conf/rsa_public_key.pem")
	expected := filepath.Clean(filepath.Join(baseDir, "rsa_public_key.pem"))
	if resolved != expected {
		t.Fatalf("expected %q, got %q", expected, resolved)
	}
}

func TestParseConfigPathDefault(t *testing.T) {
	configPath, err := parseConfigPath(nil)
	if err != nil {
		t.Fatalf("parse default config path: %v", err)
	}
	expected, err := normalizeConfigPath(defaultConfigPath)
	if err != nil {
		t.Fatalf("normalize default config path: %v", err)
	}
	if configPath != expected {
		t.Fatalf("expected default config path %q, got %q", expected, configPath)
	}
}

func TestParseConfigPathOverride(t *testing.T) {
	configPath, err := parseConfigPath([]string{"-config", "custom/config.yaml"})
	if err != nil {
		t.Fatalf("parse custom config path: %v", err)
	}
	expected, err := normalizeConfigPath("custom/config.yaml")
	if err != nil {
		t.Fatalf("normalize custom config path: %v", err)
	}
	if configPath != expected {
		t.Fatalf("expected custom config path %q, got %q", expected, configPath)
	}
}

func TestParseConfigPathRejectsBlank(t *testing.T) {
	_, err := parseConfigPath([]string{"-config", "   "})
	if err == nil {
		t.Fatal("expected blank config path to fail")
	}
}

func TestValidateConfigRejectsDuplicateAccounts(t *testing.T) {
	cfg := &Config{
		WeChat: WeChatConfig{AutoRefreshInterval: time.Minute},
		API: APIConfig{
			SignExpireHours: 1,
			Clients:         []APIClient{{AppID: "client-1", Secret: "secret", Status: "enabled"}},
		},
		Accounts: []Account{
			{AppID: "wx-1", AppSecret: "secret-a"},
			{AppID: "wx-1", AppSecret: "secret-b"},
		},
	}

	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "重复配置") {
		t.Fatalf("expected duplicate account error, got %v", err)
	}
}

func TestValidateConfigRejectsDuplicateClients(t *testing.T) {
	cfg := &Config{
		WeChat: WeChatConfig{AutoRefreshInterval: time.Minute},
		API: APIConfig{
			SignExpireHours: 1,
			Clients: []APIClient{
				{AppID: "client-1", Secret: "secret-a", Status: "enabled"},
				{AppID: "client-1", Secret: "secret-b", Status: "enabled"},
			},
		},
		Accounts: []Account{{AppID: "wx-1", AppSecret: "secret-a"}},
	}

	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "重复配置") {
		t.Fatalf("expected duplicate client error, got %v", err)
	}
}

func TestVerifySignatureRejectsFutureTimestamp(t *testing.T) {
	privateKey, publicKey := mustGenerateKeyPair(t)
	rsaPublicKey = publicKey
	logger = zap.NewNop()
	clientCache = map[string]APIClient{
		"client-1": {AppID: "client-1", Secret: "secret-1", Status: "enabled"},
	}
	cfg = Config{API: APIConfig{SignExpireHours: 2}}

	timestamp := time.Now().Add(signatureClockSkew + time.Minute).Unix()
	signature := mustSignTimestamp(t, privateKey, "client-1", "secret-1", timestamp)

	code, err := verifySignature("client-1", signature, toTimestamp(timestamp))
	if err == nil {
		t.Fatal("expected future timestamp to be rejected")
	}
	if code != CodeUnauthorized {
		t.Fatalf("expected unauthorized code, got %d", code)
	}
	if !strings.Contains(err.Error(), "时钟偏差") {
		t.Fatalf("expected clock skew error, got %v", err)
	}
}

func TestVerifySignatureAcceptsValidTimestamp(t *testing.T) {
	privateKey, publicKey := mustGenerateKeyPair(t)
	rsaPublicKey = publicKey
	logger = zap.NewNop()
	clientCache = map[string]APIClient{
		"client-1": {AppID: "client-1", Secret: "secret-1", Status: "enabled"},
	}
	cfg = Config{API: APIConfig{SignExpireHours: 2}}

	timestamp := time.Now().Unix()
	signature := mustSignTimestamp(t, privateKey, "client-1", "secret-1", timestamp)

	code, err := verifySignature("client-1", signature, toTimestamp(timestamp))
	if err != nil {
		t.Fatalf("expected valid signature, got %v", err)
	}
	if code != CodeSuccess {
		t.Fatalf("expected success code, got %d", code)
	}
}

func TestLoadRSAPublicKeyFromFile(t *testing.T) {
	privateKey, publicKey := mustGenerateKeyPair(t)
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	keyFile, err := os.CreateTemp(t.TempDir(), "rsa-public-*.pem")
	if err != nil {
		t.Fatalf("create temp key file: %v", err)
	}
	if _, err := keyFile.Write(publicPEM); err != nil {
		t.Fatalf("write temp key file: %v", err)
	}
	if err := keyFile.Close(); err != nil {
		t.Fatalf("close temp key file: %v", err)
	}

	logger = zap.NewNop()
	cfg = Config{API: APIConfig{RSAPublicKey: keyFile.Name()}}
	if err := loadRSAPublicKey(); err != nil {
		t.Fatalf("load rsa public key from file: %v", err)
	}

	timestamp := time.Now().Unix()
	clientCache = map[string]APIClient{
		"client-1": {AppID: "client-1", Secret: "secret-1", Status: "enabled"},
	}
	cfg.API.SignExpireHours = 2
	signature := mustSignTimestamp(t, privateKey, "client-1", "secret-1", timestamp)
	if code, err := verifySignature("client-1", signature, toTimestamp(timestamp)); err != nil || code != CodeSuccess {
		t.Fatalf("verify signature with loaded file key: code=%d err=%v", code, err)
	}
}

func TestIsFatalWeChatError(t *testing.T) {
	if !isFatalWeChatError(assertErr("微信API错误: errcode=40013, errmsg=invalid appid")) {
		t.Fatal("expected fatal errcode to be detected")
	}
	if isFatalWeChatError(assertErr("temporary upstream timeout")) {
		t.Fatal("expected transient error to remain retryable")
	}
}

func TestBuildLogWriteSyncerReturnsDailyWriterForDirectory(t *testing.T) {
	writer, closer, err := buildLogWriteSyncer(LoggerConfig{OutputPath: filepath.Join(t.TempDir(), "logs")})
	if err != nil {
		t.Fatalf("build log write syncer: %v", err)
	}
	t.Cleanup(func() {
		if closer != nil {
			_ = closer.Close()
		}
	})
	if writer == nil {
		t.Fatal("expected write syncer")
	}
	if _, ok := closer.(*dailyRotateWriter); !ok {
		t.Fatalf("expected dailyRotateWriter closer, got %T", closer)
	}
}

func TestDailyRotateWriterCleanupExpired(t *testing.T) {
	logDir := t.TempDir()
	now := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	keepDay := now.AddDate(0, 0, -1)
	removeDay := now.AddDate(0, 0, -2)
	if err := os.WriteFile(buildDailyLogFilePath(logDir, "wechat-token-manager", keepDay), []byte("keep"), 0o644); err != nil {
		t.Fatalf("seed keep log: %v", err)
	}
	if err := os.WriteFile(buildDailyLogFilePath(logDir, "wechat-token-manager", removeDay), []byte("remove"), 0o644); err != nil {
		t.Fatalf("seed remove log: %v", err)
	}
	writer, err := newDailyRotateWriter(logDir, "wechat-token-manager", 0o755, 2, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new daily rotate writer: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := writer.Write([]byte("current")); err != nil {
		t.Fatalf("write current log: %v", err)
	}
	if _, err := os.Stat(buildDailyLogFilePath(logDir, "wechat-token-manager", removeDay)); !os.IsNotExist(err) {
		t.Fatalf("expected expired log removed, stat err=%v", err)
	}
	if _, err := os.Stat(buildDailyLogFilePath(logDir, "wechat-token-manager", keepDay)); err != nil {
		t.Fatalf("expected retained log to exist: %v", err)
	}
}

func TestDailyRotateWriterKeepsAllLogsWhenRetentionDisabled(t *testing.T) {
	logDir := t.TempDir()
	now := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	oldDay := now.AddDate(0, 0, -10)
	oldPath := buildDailyLogFilePath(logDir, "wechat-token-manager", oldDay)
	if err := os.WriteFile(oldPath, []byte("keep"), 0o644); err != nil {
		t.Fatalf("seed old log: %v", err)
	}
	writer, err := newDailyRotateWriter(logDir, "wechat-token-manager", 0o755, 0, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new daily rotate writer: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := io.WriteString(writer, "current"); err != nil {
		t.Fatalf("write current log: %v", err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("expected old log to remain: %v", err)
	}
}

func mustGenerateKeyPair(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	block, _ := pem.Decode(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	if block == nil {
		t.Fatal("decode generated public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse generated public key: %v", err)
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		t.Fatal("parsed public key is not rsa")
	}
	return privateKey, publicKey
}

func mustSignTimestamp(t *testing.T, privateKey *rsa.PrivateKey, appid, secret string, timestamp int64) string {
	t.Helper()
	payload := appid + "|" + secret + "|" + toTimestamp(timestamp)
	hash := sha256.Sum256([]byte(payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatalf("sign payload: %v", err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func toTimestamp(ts int64) string {
	return strconv.FormatInt(ts, 10)
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
