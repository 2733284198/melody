package influxdb

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/influxdata/influxdb/client/v2"
	"melody/config"
	"melody/logging"
	alert "melody/middleware/melody-alert"
	"melody/middleware/melody-influxdb/counter"
	"melody/middleware/melody-influxdb/gauge"
	"melody/middleware/melody-influxdb/histogram"
	"melody/middleware/melody-influxdb/middleware"
	"melody/middleware/melody-influxdb/ws"
	ginmetrics "melody/middleware/melody-metrics/gin"
	"net/http"
	"net/url"
	"os"
	"time"
)

var (
	pingTimeOut = time.Second
)

type clientWrapper struct {
	client     client.Client
	collection *ginmetrics.Metrics
	logger     logging.Logger
	config     influxdbConfig
	buf        *Buffer
	timer      *ws.TimeControl
}

func Register(ctx context.Context, cfg *config.ServiceConfig, metrics *ginmetrics.Metrics, logger logging.Logger) error {
	config, ok := getConfig(cfg.ExtraConfig).(influxdbConfig)
	if !ok {
		logger.Debug("no config for the influxDB client. Aborting")
		return configErr
	}

	influxClient, err := client.NewHTTPClient(client.HTTPConfig{
		Addr:     config.address,
		Username: config.username,
		Password: config.password,
		Timeout:  config.timeout,
	})

	if err != nil {
		logger.Debug("create influx client err")
		return err
	}

	// 检察influx server是否宕机
	duration, msg, err := influxClient.Ping(pingTimeOut)
	if err != nil {
		logger.Error("unable to ping influx server,", err.Error())
		return err
	}
	logger.Debug("ping success to influx server with duration:", duration, " and message:", msg)

	t := time.NewTicker(config.ttl)

	clientWrapper := &clientWrapper{
		client:     influxClient,
		collection: metrics,
		logger:     logger,
		config:     config,
		buf:        NewBuffer(config.bufferSize),
	}

	if config.dataServerEnable {
		ws.RegisterWSTimeControl()
		// Create melody data server
		clientWrapper.runEndpoint(ctx, clientWrapper.newEngine(cfg), logger)

		// Create melody data websocket server
		clientWrapper.runWebSocketServer(ctx, cfg, logger)
	}

	checker, err := alert.NewChecker(cfg)
	if err != nil {
		logger.Error(err)
	}

	go clientWrapper.updateAndSendData(ctx, t.C, checker)

	logger.Debug("influx client run success")

	return nil
}

func (cw *clientWrapper) runWebSocketServer(ctx context.Context, cfg *config.ServiceConfig, logger logging.Logger) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	wsc := ws.WebSocketClient{
		Client:   cw.client,
		Upgrader: upgrader,
		Logger:   cw.logger,
		DB:       cw.config.db,
		Cfg:      cfg,
	}

	wsc.RegisterHandleFunc()

	go func() {
		u := url.URL{
			Scheme: "ws",
			Host:   dataServerDefaultWebSocketPort,
		}
		logger.Debug("melody data websocket server run on ", u.String(), "🎁")
		logger.Error(http.ListenAndServe(dataServerDefaultWebSocketPort, nil))
	}()
}

func (cw *clientWrapper) runEndpoint(ctx context.Context, engine *gin.Engine, logger logging.Logger) {
	server := &http.Server{
		Addr:    cw.config.dataServerPort,
		Handler: engine,
	}

	go func() {
		logger.Info("melody data server listening on port:", cw.config.dataServerPort, "🎁")
		logger.Error(server.ListenAndServe())
	}()

	go func() {
		<-ctx.Done()
		logger.Info("shutting down the melody data server")
		c, cancel := context.WithTimeout(ctx, time.Second)
		server.Shutdown(c)
		cancel()
	}()
}

func (cw *clientWrapper) newEngine(cfg *config.ServiceConfig) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	engine.Use(gin.Recovery())
	// 例: /fo/ -> /fo
	engine.RedirectTrailingSlash = true
	// 例: /../fo -> /fo
	engine.RedirectFixedPath = true
	engine.HandleMethodNotAllowed = true
	engine.Use(middleware.Cors())
	engine.POST("/ping", cw.Ping())
	if cw.config.dataServerQueryEnable {
		engine.POST("/query", cw.Query())
	}
	engine.POST("/time", cw.ModifyTimeControl())
	engine.POST("/backends", cw.Backends(cfg))
	engine.POST("/changeStatus", cw.ChangeStatus())

	return engine
}

func (cw *clientWrapper) updateAndSendData(ctx context.Context, ticker <-chan time.Time, checker alert.Checker) {
	hostname, err := os.Hostname()
	if err != nil {
		cw.logger.Error("influx client get hostname err:", err)
	}

	// 循环挂起，监听时间窗口
	for {
		select {
		case <-ticker:
		case <-ctx.Done():
			return
		}

		cw.logger.Debug("preparing get metrics points")
		snapshot := cw.collection.GetSnapshot()

		if shouldSend := len(snapshot.Counters) > 0 || len(snapshot.Gauges) > 0; !shouldSend {
			cw.logger.Debug("no metrics data to send to influx server")
			continue
		}

		bp, _ := client.NewBatchPoints(client.BatchPointsConfig{
			Precision: "s",
			Database:  cw.config.db,
		})
		now := time.Unix(0, snapshot.Time)

		for _, p := range counter.Points(hostname, now, snapshot.Counters, cw.logger, checker) {
			bp.AddPoint(p)
		}

		for _, p := range gauge.Points(hostname, now, snapshot.Gauges, cw.logger, checker) {
			bp.AddPoint(p)
		}

		for _, p := range histogram.Points(hostname, now, snapshot.Histograms, cw.logger, checker) {
			bp.AddPoint(p)
		}

		if err := cw.client.Write(bp); err != nil {
			cw.logger.Error("writing to influx server error:", err.Error())
			cw.buf.Add(bp)
			continue
		}

		cw.logger.Info(len(bp.Points()), "datapoints sent to Influx")

		var pts []*client.Point
		bpPending := cw.buf.Elements()
		for _, failedBP := range bpPending {
			pts = append(pts, failedBP.Points()...)
		}

		retryBatch, _ := client.NewBatchPoints(client.BatchPointsConfig{
			Database:  cw.config.db,
			Precision: "s",
		})
		retryBatch.AddPoints(pts)

		if err := cw.client.Write(retryBatch); err != nil {
			cw.logger.Error("writing to influx:", err.Error())
			cw.buf.Add(bpPending...)
			continue
		}
	}
}
