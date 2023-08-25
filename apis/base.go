// Package apis implements the default Space api services and middlewares.
package apis

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/unkod/space/core"
	"github.com/unkod/space/tools/rest"
)

// InitApi creates a configured echo instance with registered
// system and app specific routes and middlewares.
func InitApi(app core.App) (*echo.Echo, error) {
	e := echo.New()
	e.Debug = app.IsDebug()
	e.JSONSerializer = &rest.Serializer{
		FieldsParam: "fields",
	}

	// configure a custom router
	e.ResetRouterCreator(func(ec *echo.Echo) echo.Router {
		return echo.NewRouter(echo.RouterConfig{
			UnescapePathParamValues: true,
			AllowOverwritingRoute:   true,
		})
	})

	// default middlewares
	e.Pre(middleware.RemoveTrailingSlashWithConfig(middleware.RemoveTrailingSlashConfig{
		Skipper: func(c echo.Context) bool {
			// enable by default only for the API routes
			return !strings.HasPrefix(c.Request().URL.Path, "/api/")
		},
	}))
	e.Pre(LoadAuthContext(app))
	e.Use(middleware.Recover())
	e.Use(middleware.Secure())

	// custom error handler
	e.HTTPErrorHandler = func(c echo.Context, err error) {
		if err == nil {
			return // no error
		}

		if c.Response().Committed {
			if app.IsDebug() {
				log.Println("HTTPErrorHandler response was already committed:", err)
			}
			return
		}

		var apiErr *ApiError

		if errors.As(err, &apiErr) {
			if app.IsDebug() && apiErr.RawData() != nil {
				log.Println(apiErr.RawData())
			}
		} else if v := new(echo.HTTPError); errors.As(err, &v) {
			if v.Internal != nil && app.IsDebug() {
				log.Println(v.Internal)
			}
			msg := fmt.Sprintf("%v", v.Message)
			apiErr = NewApiError(v.Code, msg, v)
		} else {
			if app.IsDebug() {
				log.Println(err)
			}

			if errors.Is(err, sql.ErrNoRows) {
				apiErr = NewNotFoundError("", err)
			} else {
				apiErr = NewBadRequestError("", err)
			}
		}

		event := new(core.ApiErrorEvent)
		event.HttpContext = c
		event.Error = apiErr

		// send error response
		hookErr := app.OnBeforeApiError().Trigger(event, func(e *core.ApiErrorEvent) error {
			if c.Response().Committed {
				return nil
			}

			// @see https://github.com/labstack/echo/issues/608
			if e.HttpContext.Request().Method == http.MethodHead {
				return e.HttpContext.NoContent(apiErr.Code)
			}

			return e.HttpContext.JSON(apiErr.Code, apiErr)
		})

		if hookErr == nil {
			if err := app.OnAfterApiError().Trigger(event); err != nil && app.IsDebug() {
				log.Println(hookErr)
			}
		} else if app.IsDebug() {
			// truly rare case; eg. client already disconnected
			log.Println(hookErr)
		}
	}

	// default routes
	api := e.Group("/api", eagerRequestInfoCache(app))
	bindSettingsApi(app, api)
	bindAdminApi(app, api)
	bindCollectionApi(app, api)
	bindRecordCrudApi(app, api)
	bindRecordAuthApi(app, api)
	bindFileApi(app, api)
	bindRealtimeApi(app, api)
	bindLogsApi(app, api)
	bindHealthApi(app, api)
	bindBackupApi(app, api)

	// catch all any route
	api.Any("/*", func(c echo.Context) error {
		return echo.ErrNotFound
	}, ActivityLogger(app))

	return e, nil
}

// StaticDirectoryHandler is similar to `echo.StaticDirectoryHandler`
// but without the directory redirect which conflicts with RemoveTrailingSlash middleware.
//
// If a file resource is missing and indexFallback is set, the request
// will be forwarded to the base index.html (useful also for SPA).
//
// @see https://github.com/labstack/echo/issues/2211
func StaticDirectoryHandler(fileSystem fs.FS, indexFallback bool) echo.HandlerFunc {
	return func(c echo.Context) error {
		p := c.PathParam("*")

		// escape url path
		tmpPath, err := url.PathUnescape(p)
		if err != nil {
			return fmt.Errorf("failed to unescape path variable: %w", err)
		}
		p = tmpPath

		// fs.FS.Open() already assumes that file names are relative to FS root path and considers name with prefix `/` as invalid
		name := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(p, "/")))

		fileErr := c.FileFS(name, fileSystem)

		if fileErr != nil && indexFallback && errors.Is(fileErr, echo.ErrNotFound) {
			return c.FileFS("index.html", fileSystem)
		}

		return fileErr
	}
}
