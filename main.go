package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	realip "github.com/Ferluci/fast-realip"
	"github.com/JasonKhew96/biliroaming-go-server/database"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
	"golang.org/x/net/idna"
	"golang.org/x/time/rate"
)

// arguments struct
type BiliArgs struct {
	accessKey string
	area      string
	cid       string
	epId      string
	seasonId  string
	keyword   string
}

// ip string
type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// BiliroamingGo ...
type BiliroamingGo struct {
	config   *Config
	visitors map[string]*visitor
	vMu      sync.RWMutex
	ctx      context.Context
	logger   *zap.Logger
	sugar    *zap.SugaredLogger

	cnClient      *fasthttp.Client
	hkClient      *fasthttp.Client
	twClient      *fasthttp.Client
	thClient      *fasthttp.Client
	defaultClient *fasthttp.Client

	db *database.Database
}

var reMid = regexp.MustCompile(`(&|\\u0026)mid=\d+`)

// get visitor limiter
func (b *BiliroamingGo) getVisitor(ip string) *rate.Limiter {
	b.vMu.Lock()
	defer b.vMu.Unlock()
	u, exists := b.visitors[ip]
	if !exists {
		uLimiter := rate.NewLimiter(rate.Limit(b.config.IPLimit), b.config.IPBurst)
		b.visitors[ip] = &visitor{
			limiter: uLimiter,
		}
		return uLimiter
	}

	u.lastSeen = time.Now()
	return u.limiter
}

func (b *BiliroamingGo) cleanupDatabase() {
	for {
		b.sugar.Debug("Cleaning database...")
		if aff, err := b.db.CleanupAccessKeys(time.Duration(b.config.CacheAccessKey) * 24 * time.Hour); err != nil {
			b.sugar.Error(err)
		} else {
			b.sugar.Debugf("Cleanup %d access keys cache", aff)
		}
		if aff, err := b.db.CleanupUsers(time.Duration(b.config.CacheUser) * 24 * time.Hour); err != nil {
			b.sugar.Error(err)
		} else {
			b.sugar.Debugf("Cleanup %d users cache", aff)
		}
		if aff, err := b.db.CleanupPlayURLCache(time.Duration(b.config.CachePlayURL) * time.Minute); err != nil {
			b.sugar.Error(err)
		} else {
			b.sugar.Debugf("Cleanup %d playURL cache", aff)
		}
		if aff, err := b.db.CleanupTHSeasonCache(time.Duration(b.config.CacheTHSeason) * time.Minute); err != nil {
			b.sugar.Error(err)
		} else {
			b.sugar.Debugf("Cleanup %d TH season cache", aff)
		}
		if aff, err := b.db.CleanupTHSubtitleCache(time.Duration(b.config.CacheTHSubtitle) * time.Minute); err != nil {
			b.sugar.Error(err)
		} else {
			b.sugar.Debugf("Cleanup %d TH subtitle cache", aff)
		}

		// cleanup ip cache
		b.vMu.Lock()
		for ip, v := range b.visitors {
			if time.Since(v.lastSeen) > 5*time.Minute {
				delete(b.visitors, ip)
			}
		}
		b.vMu.Unlock()

		time.Sleep(5 * time.Minute)
	}
}

func main() {
	// default config
	c, err := initConfig()
	if err != nil {
		panic(err)
	}

	var logger *zap.Logger
	if c.Debug {
		logger, err = zap.NewDevelopment()
		if err != nil {
			panic(err)
		}
	} else {
		logger, err = zap.NewProduction()
		if err != nil {
			panic(err)
		}
	}
	sugar := logger.Sugar()

	b := BiliroamingGo{
		config:   c,
		visitors: make(map[string]*visitor),
		ctx:      context.Background(),
		logger:   logger,
		sugar:    sugar,
	}

	cnClient, hkClient, twClient, thClient, defaultClient := b.initProxy(b.config)
	b.cnClient = cnClient
	b.hkClient = hkClient
	b.twClient = twClient
	b.thClient = thClient
	b.defaultClient = defaultClient

	pgPassword := c.PGPassword
	if c.PGPasswordFile != "" {
		data, err := os.ReadFile(c.PGPasswordFile)
		if err != nil {
			b.sugar.Fatal(err)
		}
		if len(data) > 0 {
			pgPassword = string(data)
		}
	}

	b.db, err = database.NewDBConnection(&database.Config{
		Host:     c.PGHost,
		User:     c.PGUser,
		Password: pgPassword,
		DBName:   c.PGDBName,
		Port:     c.PGPort,
	})
	if err != nil {
		b.sugar.Fatal(err)
	}

	go b.cleanupDatabase()
	// go b.cleanupVisitors()

	fs := &fasthttp.FS{
		Root:               "html",
		IndexNames:         []string{"index.html"},
		GenerateIndexPages: true,
		Compress:           true,
		AcceptByteRange:    false,
		PathNotFound:       processNotFound,
		// PathRewrite:        fasthttp.NewVHostPathRewriter(0),
	}
	fsHandler := fs.NewRequestHandler()

	mux := func(ctx *fasthttp.RequestCtx) {
		clientIP := realip.FromRequest(ctx)
		limiter := b.getVisitor(clientIP)
		if !limiter.Allow() {
			ctx.Error(fasthttp.StatusMessage(fasthttp.StatusTooManyRequests), fasthttp.StatusTooManyRequests)
			return
		}

		switch string(ctx.Path()) {
		case "/pgc/player/web/playurl": // web
			b.handleWebPlayURL(ctx)
		case "/x/v2/search/type": // web
			b.handleWebSearch(ctx)
		case "/pgc/player/api/playurl": // android
			b.handleAndroidPlayURL(ctx)
		case "/intl/gateway/v2/app/search/type": // bstar android
			b.handleBstarAndroidSearch(ctx)
		case "/intl/gateway/v2/ogv/view/app/season": // bstar android
			b.handleBstarAndroidSeason(ctx)
		case "/intl/gateway/v2/app/subtitle": // bstar android
			b.handleBstarAndroidSubtitle(ctx)
		case "/intl/gateway/v2/ogv/playurl": // bstar android
			b.handleBstarAndroidPlayURL(ctx)
		default:
			fsHandler(ctx)
			// ctx.Error(fasthttp.StatusMessage(fasthttp.StatusNotFound), fasthttp.StatusNotFound)
		}
	}

	sugar.Infof("Listening on :%d ...", c.Port)
	err = fasthttp.ListenAndServe(":"+strconv.Itoa(c.Port), mux)
	if err != nil {
		sugar.Panic(err)
	}
}

func processNotFound(ctx *fasthttp.RequestCtx) {
	ctx.Error(fasthttp.StatusMessage(fasthttp.StatusNotFound), fasthttp.StatusNotFound)
}

func (b *BiliroamingGo) processError(ctx *fasthttp.RequestCtx, err error) {
	b.sugar.Error(err)
	ctx.Error(
		fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
		fasthttp.StatusInternalServerError,
	)
}

func (b *BiliroamingGo) processArgs(args *fasthttp.Args) *BiliArgs {
	queryArgs := &BiliArgs{
		accessKey: string(args.Peek("access_key")),
		area:      string(args.Peek("area")),
		cid:       string(args.Peek("cid")),
		epId:      string(args.Peek("ep_id")),
		seasonId:  string(args.Peek("season_id")),
		keyword:   string(args.Peek("keyword")),
	}
	b.sugar.Debug("Request args ", args.String())
	b.sugar.Debugf(
		"Parsed request args: %v",
		queryArgs,
	)
	return queryArgs
}

func (b *BiliroamingGo) doAuth(ctx *fasthttp.RequestCtx, accessKey, area string) (bool, bool) {
	if len(accessKey) != 32 {
		writeErrorJSON(ctx, -2, []byte("Access Key错误"))
		return false, false
	}

	isAuth, isVIP, err := b.isAuth(ctx.Request.Header.UserAgent(), accessKey)
	if err != nil {
		b.sugar.Error(err)
		writeErrorJSON(ctx, -500, []byte("服务器错误"))
		return false, false
	}
	if !isAuth {
		writeErrorJSON(ctx, -101, []byte("账号未登录"))
		return false, false
	}
	return true, isVIP
}

func (b *BiliroamingGo) handleWebPlayURL(ctx *fasthttp.RequestCtx) {
	queryArgs := ctx.URI().QueryArgs()
	args := b.processArgs(queryArgs)

	if args.area == "" {
		writeErrorJSON(ctx, -688, []byte("地理区域限制"))
		return
	}

	cidInt, err := strconv.Atoi(args.cid)
	if err != nil {
		b.processError(ctx, err)
		return
	}
	epidInt, err := strconv.Atoi(args.epId)
	if err != nil {
		b.processError(ctx, err)
		return
	}

	var isVIP bool
	if b.getAuthByArea(args.area) {
		var ok bool
		if ok, isVIP = b.doAuth(ctx, args.accessKey, args.area); !ok {
			return
		}
		playurlCache, err := b.db.GetPlayURLCache(database.DeviceTypeWeb, getAreaCode(args.area), isVIP, cidInt, epidInt)
		if err == nil && playurlCache.JSONData != "" && playurlCache.UpdatedAt.Before(time.Now().Add(time.Hour)) {
			b.sugar.Debug("Replay from cache: ", playurlCache.JSONData)
			setDefaultHeaders(ctx)
			ctx.Write([]byte(playurlCache.JSONData))
			return
		}
	}

	client := b.getClientByArea(args.area)

	v := url.Values{}
	v.Set("access_key", args.accessKey)
	v.Set("area", args.area)
	v.Set("cid", args.cid)
	v.Set("ep_id", args.epId)
	v.Set("fnver", "0")
	v.Set("fnval", "80")
	v.Set("fourk", "1")
	// v.Set("qn", "120")

	params, err := SignParams(v, ClientTypeAndroid)
	if err != nil {
		b.processError(ctx, err)
		return
	}

	reverseProxy := b.getReverseProxyByArea(args.area)
	if reverseProxy == "" {
		reverseProxy = "api.bilibili.com"
	}
	domain, err := idna.New().ToASCII(reverseProxy)
	if err != nil {
		b.processError(ctx, err)
		return
	}

	url := fmt.Sprintf("https://%s/pgc/player/web/playurl?%s", domain, params)
	b.sugar.Debug("New url: ", url)

	data := b.doRequestWrite(ctx, client, url)
	if data != nil && b.getAuthByArea(args.area) {
		b.db.InsertOrUpdatePlayURLCache(database.DeviceTypeWeb, getAreaCode(args.area), isVIP, cidInt, epidInt, string(data))
	}
}

func (b *BiliroamingGo) handleWebSearch(ctx *fasthttp.RequestCtx) {
	queryArgs := ctx.URI().QueryArgs()
	args := b.processArgs(queryArgs)

	if args.area == "" {
		writeErrorJSON(ctx, -688, []byte("地理区域限制"))
		return
	}

	client := b.getClientByArea(args.area)

	v := url.Values{}
	v.Set("access_key", args.accessKey)
	v.Set("area", args.area)
	v.Set("type", "7")
	v.Set("build", "6400000")
	v.Set("mobi_app", "android")
	v.Set("platform", "android")

	params, err := SignParams(v, ClientTypeAndroid)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	reverseProxy := b.getReverseProxyByArea(args.area)
	if reverseProxy == "" {
		reverseProxy = "api.bilibili.com"
	}
	domain, err := idna.New().ToASCII(reverseProxy)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	url := fmt.Sprintf("https://%s/x/v2/search/type?%s", domain, params)
	b.sugar.Debug("New url: ", url)

	b.doRequestWrite(ctx, client, url)
}

func (b *BiliroamingGo) handleAndroidPlayURL(ctx *fasthttp.RequestCtx) {
	queryArgs := ctx.URI().QueryArgs()
	args := b.processArgs(queryArgs)
	client := b.getClientByArea(args.area)

	if args.area == "" {
		writeErrorJSON(ctx, -688, []byte("地理区域限制"))
		return
	}

	cidInt, err := strconv.Atoi(args.cid)
	if err != nil {
		b.processError(ctx, err)
		return
	}
	epidInt, err := strconv.Atoi(args.epId)
	if err != nil {
		b.processError(ctx, err)
		return
	}

	var isVIP bool
	if b.getAuthByArea(args.area) {
		var ok bool
		if ok, isVIP = b.doAuth(ctx, args.accessKey, args.area); !ok {
			return
		}

		playurlCache, err := b.db.GetPlayURLCache(database.DeviceTypeAndroid, getAreaCode(args.area), isVIP, cidInt, epidInt)
		if err == nil && playurlCache.JSONData != "" && playurlCache.UpdatedAt.Before(time.Now().Add(time.Hour)) {
			b.sugar.Debug("Replay from cache: ", playurlCache.JSONData)
			setDefaultHeaders(ctx)
			ctx.Write([]byte(playurlCache.JSONData))
			return
		}
	}

	v := url.Values{}
	v.Set("access_key", args.accessKey)
	v.Set("area", args.area)
	v.Set("cid", args.cid)
	v.Set("ep_id", args.epId)
	v.Set("fnver", "0")
	v.Set("fnval", "80")
	v.Set("fourk", "1")
	v.Set("platform", "android")
	// v.Set("qn", "120")

	params, err := SignParams(v, ClientTypeAndroid)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	reverseProxy := b.getReverseProxyByArea(args.area)
	if reverseProxy == "" {
		reverseProxy = "api.bilibili.com"
	}
	domain, err := idna.New().ToASCII(reverseProxy)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	url := fmt.Sprintf("https://%s/pgc/player/api/playurl?%s", domain, params)
	b.sugar.Debug("New url: ", url)

	data := b.doRequestWrite(ctx, client, url)
	if data != nil && b.getAuthByArea(args.area) {
		b.db.InsertOrUpdatePlayURLCache(database.DeviceTypeAndroid, getAreaCode(args.area), isVIP, cidInt, epidInt, string(data))
	}
}

func (b *BiliroamingGo) handleBstarAndroidSearch(ctx *fasthttp.RequestCtx) {
	queryArgs := ctx.URI().QueryArgs()
	args := b.processArgs(queryArgs)

	if args.area == "" {
		args.area = "th"
		// writeErrorJSON(ctx, -688, []byte("地理区域限制"))
		// return
	}

	client := b.getClientByArea(args.area)

	if args.keyword == "" {
		writeErrorJSON(ctx, -400, []byte("请求错误"))
		return
	}

	if b.getAuthByArea(args.area) {
		if ok, _ := b.doAuth(ctx, args.accessKey, args.area); !ok {
			return
		}
	}

	v := url.Values{}
	v.Set("access_key", args.accessKey)
	v.Set("area", args.area)
	v.Set("build", "1080003")
	v.Set("s_locale", "zh_SG")
	v.Set("type", "7")
	v.Set("mobi_app", "bstar_a")

	params, err := SignParams(v, ClientTypeBstarA)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	reverseProxy := b.getReverseProxyByArea(args.area)
	if reverseProxy == "" {
		reverseProxy = "api.biliintl.com"
	}
	domain, err := idna.New().ToASCII(reverseProxy)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	url := fmt.Sprintf("https://%s/intl/gateway/v2/app/search/type?%s", domain, params)
	b.sugar.Debug("New url: ", url)

	b.doRequestWrite(ctx, client, url)
}

func (b *BiliroamingGo) handleBstarAndroidSeason(ctx *fasthttp.RequestCtx) {
	queryArgs := ctx.URI().QueryArgs()
	args := b.processArgs(queryArgs)

	if args.area == "" {
		args.area = "th"
		// writeErrorJSON(ctx, -688, []byte("地理区域限制"))
		// return
	}

	client := b.getClientByArea(args.area)

	if args.seasonId == "" {
		writeErrorJSON(ctx, -400, []byte("请求错误"))
		return
	}

	seasonIdInt, err := strconv.Atoi(args.seasonId)
	if err != nil {
		b.processError(ctx, err)
		return
	}

	if b.getAuthByArea(args.area) {
		if ok, _ := b.doAuth(ctx, args.accessKey, args.area); !ok {
			return
		}
		seasonCache, err := b.db.GetTHSeasonCache(seasonIdInt)
		if err == nil && seasonCache.JSONData != "" && seasonCache.UpdatedAt.Before(time.Now().Add(15*time.Minute)) {
			b.sugar.Debug("Replay from cache: ", seasonCache.JSONData)
			setDefaultHeaders(ctx)
			ctx.Write([]byte(seasonCache.JSONData))
			return
		}
	}

	v := url.Values{}
	v.Set("access_key", args.accessKey)
	v.Set("area", args.area)
	v.Set("build", "1080003")
	v.Set("s_locale", "zh_SG")
	v.Set("season_id", args.seasonId)
	v.Set("mobi_app", "bstar_a")

	params, err := SignParams(v, ClientTypeBstarA)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	reverseProxy := b.getReverseProxyByArea(args.area)
	if reverseProxy == "" {
		reverseProxy = "api.biliintl.com"
	}
	domain, err := idna.New().ToASCII(reverseProxy)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	url := fmt.Sprintf("https://%s/intl/gateway/v2/ogv/view/app/season?%s", domain, params)
	b.sugar.Debug("New url: ", url)

	data := b.doRequestWrite(ctx, client, url)
	if data != nil && b.getAuthByArea(args.area) {
		b.db.InsertOrUpdateTHSeasonCache(seasonIdInt, string(data))
	}
}

func (b *BiliroamingGo) handleBstarAndroidSubtitle(ctx *fasthttp.RequestCtx) {
	queryArgs := ctx.URI().QueryArgs()
	args := b.processArgs(queryArgs)

	if args.area == "" {
		args.area = "th"
		// writeErrorJSON(ctx, -688, []byte("地理区域限制"))
		// return
	}

	client := b.getClientByArea(args.area)

	episodeIdInt, err := strconv.Atoi(args.epId)
	if err != nil {
		b.processError(ctx, err)
		return
	}

	if b.getAuthByArea(args.area) {
		// if ok, _ := b.doAuth(ctx, accessKey, area); !ok {
		// 	return
		// }
		subtitleCache, err := b.db.GetTHSubtitleCache(episodeIdInt)
		if err == nil && subtitleCache.JSONData != "" && subtitleCache.UpdatedAt.Before(time.Now().Add(15*time.Minute)) {
			b.sugar.Debug("Replay from cache: ", subtitleCache.JSONData)
			setDefaultHeaders(ctx)
			ctx.Write([]byte(subtitleCache.JSONData))
			return
		}
	}

	v := url.Values{}
	v.Set("access_key", args.accessKey)
	v.Set("area", args.area)
	v.Set("s_locale", "zh_SG")
	v.Set("ep_id", args.epId)
	v.Set("mobi_app", "bstar_a")

	params, err := SignParams(v, ClientTypeBstarA)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	reverseProxy := b.getReverseProxyByArea(args.area)
	if reverseProxy == "" {
		reverseProxy = "app.biliintl.com"
	}
	domain, err := idna.New().ToASCII(reverseProxy)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	url := fmt.Sprintf("https://%s/intl/gateway/v2/app/subtitle?%s", domain, params)
	b.sugar.Debug("New url: ", url)

	data := b.doRequestWrite(ctx, client, url)
	if data != nil && b.getAuthByArea(args.area) {
		b.db.InsertOrUpdateTHSubtitleCache(episodeIdInt, string(data))
	}
}

func (b *BiliroamingGo) handleBstarAndroidPlayURL(ctx *fasthttp.RequestCtx) {
	queryArgs := ctx.URI().QueryArgs()
	args := b.processArgs(queryArgs)

	if args.area == "" {
		args.area = "th"
		// writeErrorJSON(ctx, -688, []byte("地理区域限制"))
		// return
	}

	client := b.getClientByArea(args.area)

	cidInt, err := strconv.Atoi(args.cid)
	if err != nil {
		b.processError(ctx, err)
		return
	}
	epidInt, err := strconv.Atoi(args.epId)
	if err != nil {
		b.processError(ctx, err)
		return
	}

	var isVIP bool
	if b.getAuthByArea(args.area) {
		var ok bool
		if ok, isVIP = b.doAuth(ctx, args.accessKey, args.area); !ok {
			return
		}

		playurlCache, err := b.db.GetPlayURLCache(database.DeviceTypeAndroid, getAreaCode(args.area), isVIP, cidInt, epidInt)
		if err == nil && playurlCache.JSONData != "" && playurlCache.UpdatedAt.Before(time.Now().Add(time.Hour)) {
			b.sugar.Debug("Replay from cache: ", playurlCache.JSONData)
			setDefaultHeaders(ctx)
			ctx.Write([]byte(playurlCache.JSONData))
			return
		}
	}

	v := url.Values{}
	v.Set("access_key", args.accessKey)
	v.Set("area", args.area)
	v.Set("cid", args.cid)
	v.Set("ep_id", args.epId)
	v.Set("fnver", "0")
	v.Set("fnval", "80")
	v.Set("fourk", "1")
	v.Set("platform", "android")
	v.Set("s_locale", "zh_SG")
	// v.Set("qn", "120")

	params, err := SignParams(v, ClientTypeBstarA)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	reverseProxy := b.getReverseProxyByArea(args.area)
	if reverseProxy == "" {
		reverseProxy = "api.biliintl.com"
	}
	domain, err := idna.New().ToASCII(reverseProxy)
	if err != nil {
		b.sugar.Error(err)
		ctx.Error(
			fasthttp.StatusMessage(fasthttp.StatusInternalServerError),
			fasthttp.StatusInternalServerError,
		)
		return
	}

	url := fmt.Sprintf("https://%s/intl/gateway/v2/ogv/playurl?%s", domain, params)
	b.sugar.Debug("New url: ", url)

	data := b.doRequestWrite(ctx, client, url)
	if data != nil && b.getAuthByArea(args.area) {
		b.db.InsertOrUpdatePlayURLCache(database.DeviceTypeAndroid, getAreaCode(args.area), isVIP, cidInt, epidInt, string(data))
	}
}
