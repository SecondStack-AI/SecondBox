package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/observability"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (apiHandler *handler) getSandboxTiming(
	writer http.ResponseWriter,
	request *http.Request,
) {
	limit, err := requiredBoundedTimingQuery(request, "limit", 1, 200)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	timing, err := apiHandler.service.GetSandboxTiming(
		request.Context(), requestPrincipal(request),
		request.PathValue("sandboxID"), int(limit),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, timing)
}

func (apiHandler *handler) getOperationTiming(
	writer http.ResponseWriter,
	request *http.Request,
) {
	timing, err := apiHandler.service.GetOperationTiming(
		request.Context(), requestPrincipal(request), request.PathValue("operationID"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, timing)
}

func (apiHandler *handler) getDeploymentTiming(
	writer http.ResponseWriter,
	request *http.Request,
) {
	windowSeconds, err := requiredBoundedTimingQuery(
		request, "windowSeconds", 60, 3600,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	summary, err := apiHandler.service.GetDeploymentTiming(
		request.Context(), requestPrincipal(request), windowSeconds,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	httpSeries := apiHandler.timings.HTTPSnapshotBetween(
		summary.ObservedAt.Add(-time.Duration(summary.WindowSeconds)*time.Second),
		summary.ObservedAt,
	)
	histograms := make([]observability.DurationHistogram, 0, len(httpSeries))
	for _, series := range httpSeries {
		duration, err := publicDurationPercentiles(series.Histogram)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		summary.APISeries = append(summary.APISeries, contracts.HTTPRouteTimingSummary{
			Route: series.Route, StatusClass: series.StatusClass, Duration: duration,
		})
		histograms = append(histograms, series.Histogram)
	}
	summary.API, err = publicDurationPercentiles(
		observability.MergeHistograms(histograms),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, summary)
}

func requiredBoundedTimingQuery(
	request *http.Request,
	name string,
	minimum int64,
	maximum int64,
) (int64, error) {
	values, exists := request.URL.Query()[name]
	if !exists || len(values) != 1 || values[0] == "" {
		return 0, requestValidationError(errors.New("SecondBox timing query parameter is required: " + name))
	}
	value, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, requestValidationError(errors.New(
			"SecondBox timing query parameter is outside its explicit bound: " + name,
		))
	}
	return value, nil
}

func publicDurationPercentiles(
	histogram observability.DurationHistogram,
) (contracts.DurationPercentiles, error) {
	if histogram.Count > uint64(^uint64(0)>>1) {
		return contracts.DurationPercentiles{}, errors.New(
			"SecondBox HTTP timing count exceeds the public range",
		)
	}
	return contracts.DurationPercentiles{
		Count:           int64(histogram.Count),
		P50Milliseconds: float64Milliseconds(observability.PercentileMilliseconds(histogram, 0.50)),
		P95Milliseconds: float64Milliseconds(observability.PercentileMilliseconds(histogram, 0.95)),
		P99Milliseconds: float64Milliseconds(observability.PercentileMilliseconds(histogram, 0.99)),
	}, nil
}

func float64Milliseconds(value *int64) *float64 {
	if value == nil {
		return nil
	}
	milliseconds := float64(*value)
	return &milliseconds
}
