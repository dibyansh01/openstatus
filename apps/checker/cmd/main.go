package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/openstatushq/openstatus/apps/checker"
	"github.com/openstatushq/openstatus/apps/checker/pkg/logger"
	"github.com/openstatushq/openstatus/apps/checker/pkg/tinybird"
	"github.com/openstatushq/openstatus/apps/checker/request"
	"github.com/rs/zerolog/log"

	backoff "github.com/cenkalti/backoff/v4"
)

type statusCode int

func (s statusCode) IsSuccessful() bool {
	return s >= 200 && s < 300
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-done
		cancel()
	}()

	// environment variables.
	flyRegion := env("FLY_REGION", "local")
	cronSecret := env("CRON_SECRET", "")
	tinyBirdToken := env("TINYBIRD_TOKEN", "")
	logLevel := env("LOG_LEVEL", "warn")

	logger.Configure(logLevel)

	// packages.
	httpClient := &http.Client{}
	defer httpClient.CloseIdleConnections()

	tinybirdClient := tinybird.NewClient(httpClient, tinyBirdToken)

	router := gin.New()
	router.POST("/checker", func(c *gin.Context) {
		ctx := c.Request.Context()

		if c.GetHeader("Authorization") != fmt.Sprintf("Basic %s", cronSecret) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		var req request.CheckerRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			log.Ctx(ctx).Error().Err(err).Msg("failed to decode checker request")
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
			return
		}

		op := func() error {
			res, err := checker.Ping(ctx, httpClient, req)
			if err != nil {
				return fmt.Errorf("unable to ping: %w", err)
			}

			statusCode := statusCode(res.StatusCode)
			if !statusCode.IsSuccessful() {
				// Q: Why here we do not check if the status was previously active?
				checker.UpdateStatus(ctx, checker.UpdateData{
					MonitorId:  req.MonitorID,
					Status:     "error",
					StatusCode: res.StatusCode,
					Region:     flyRegion,
				})
			} else if req.Status == "error" && statusCode.IsSuccessful() {
				// Q: Why here we check the data before updating the status in this scenario?
				checker.UpdateStatus(ctx, checker.UpdateData{
					MonitorId:  req.MonitorID,
					Status:     "active",
					Region:     flyRegion,
					StatusCode: res.StatusCode,
				})
			}

			if err := tinybirdClient.SendEvent(ctx, res); err != nil {
				log.Ctx(ctx).Error().Err(err).Msg("failed to send event to tinybird")
			}

			return nil
		}

		if err := backoff.Retry(op, backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3)); err != nil {
			if err := tinybirdClient.SendEvent(ctx, checker.PingData{
				URL:           req.URL,
				Region:        flyRegion,
				Message:       err.Error(),
				CronTimestamp: req.CronTimestamp,
				Timestamp:     req.CronTimestamp,
				MonitorID:     req.MonitorID,
				WorkspaceID:   req.WorkspaceID,
			}); err != nil {
				log.Ctx(ctx).Error().Err(err).Msg("failed to send event to tinybird")
			}

			// If the status was previously active, we update it to error.
			// Q: Why not always updating the status? My idea is that the checker should be dumb and only check the status and return it.
			if req.Status == "active" {
				checker.UpdateStatus(ctx, checker.UpdateData{
					MonitorId: req.MonitorID,
					Status:    "error",
					Message:   err.Error(),
					Region:    flyRegion,
				})
			}
		}

		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})

	router.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong", "fly_region": flyRegion})
		return
	})

	httpServer := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%s", env("PORT", "8080")),
		Handler: router,
	}

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Ctx(ctx).Error().Err(err).Msg("failed to start http server")
			cancel()
		}
	}()

	<-ctx.Done()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Ctx(ctx).Error().Err(err).Msg("failed to shutdown http server")
		return
	}
}

func env(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}

	return fallback
}
