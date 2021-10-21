/*
Copyright 2021 The KEDA Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cache

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/kedacore/keda/v2/pkg/eventreason"
	"github.com/kedacore/keda/v2/pkg/scalers"
	"k8s.io/api/autoscaling/v2beta2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	"k8s.io/metrics/pkg/apis/external_metrics"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type ScalersCache struct {
	scalers  []scalerBuilder
	logger   logr.Logger
	recorder record.EventRecorder
}

func NewScalerCache(scalers []scalers.Scaler, factories []func() (scalers.Scaler, error), logger logr.Logger, recorder record.EventRecorder) (*ScalersCache, error) {
	if len(scalers) != len(factories) {
		return nil, fmt.Errorf("scalers and factories must match")
	}
	builders := make([]scalerBuilder, 0, len(scalers))
	for i := range scalers {
		builders = append(builders, scalerBuilder{
			scaler:  scalers[i],
			factory: factories[i],
		})
	}
	return &ScalersCache{
		scalers:  builders,
		logger:   logger,
		recorder: recorder,
	}, nil
}

type scalerBuilder struct {
	scaler  scalers.Scaler
	factory func() (scalers.Scaler, error)
}

func (c *ScalersCache) GetScalers() []scalers.Scaler {
	result := make([]scalers.Scaler, 0, len(c.scalers))
	for _, s := range c.scalers {
		result = append(result, s.scaler)
	}
	return result
}

func (c *ScalersCache) GetPushScalers() []scalers.PushScaler {
	var result []scalers.PushScaler
	for _, s := range c.scalers {
		if ps, ok := s.scaler.(scalers.PushScaler); ok {
			result = append(result, ps)
		}
	}
	return result
}

func (c *ScalersCache) GetMetricsForScaler(ctx context.Context, id int, metricName string, metricSelector labels.Selector) ([]external_metrics.ExternalMetricValue, error) {
	if id < 0 || id >= len(c.scalers) {
		return nil, fmt.Errorf("scaler with id %d not found. Len = %d", id, len(c.scalers))
	}
	m, err := c.scalers[id].scaler.GetMetrics(ctx, metricName, metricSelector)
	if err == nil {
		return m, nil
	}

	ns, err := c.refreshScaler(id)
	if err != nil {
		return nil, err
	}

	return ns.GetMetrics(ctx, metricName, metricSelector)
}

func (c *ScalersCache) IsScaledObjectActive(ctx context.Context, scaledObject *kedav1alpha1.ScaledObject) (bool, bool, []external_metrics.ExternalMetricValue) {
	isActive := false
	isError := false
	for i, s := range c.scalers {
		isTriggerActive, err := s.scaler.IsActive(ctx)
		if err != nil {
			var ns scalers.Scaler
			ns, err = c.refreshScaler(i)
			if err == nil {
				isTriggerActive, err = ns.IsActive(ctx)
			}
		}

		if err != nil {
			c.logger.V(1).Info("Error getting scale decision", "Error", err)
			isError = true
			c.recorder.Event(scaledObject, corev1.EventTypeWarning, eventreason.KEDAScalerFailed, err.Error())
		} else if isTriggerActive {
			isActive = true
			if externalMetricsSpec := s.scaler.GetMetricSpecForScaling()[0].External; externalMetricsSpec != nil {
				c.logger.V(1).Info("Scaler for scaledObject is active", "Metrics Name", externalMetricsSpec.Metric.Name)
			}
			if resourceMetricsSpec := s.scaler.GetMetricSpecForScaling()[0].Resource; resourceMetricsSpec != nil {
				c.logger.V(1).Info("Scaler for scaledObject is active", "Metrics Name", resourceMetricsSpec.Name)
			}
			break
		}
	}

	return isActive, isError, []external_metrics.ExternalMetricValue{}
}

func (c *ScalersCache) IsScaledJobActive(ctx context.Context, scaledJob *kedav1alpha1.ScaledJob) (bool, int64, int64) {
	var queueLength int64
	var maxValue int64
	isActive := false

	logger := logf.Log.WithName("scalemetrics")
	scalersMetrics := c.getScaledJobMetrics(ctx, scaledJob)
	switch scaledJob.Spec.ScalingStrategy.MultipleScalersCalculation {
	case "min":
		for _, metrics := range scalersMetrics {
			if (queueLength == 0 || metrics.queueLength < queueLength) && metrics.isActive {
				queueLength = metrics.queueLength
				maxValue = metrics.maxValue
				isActive = metrics.isActive
			}
		}
	case "avg":
		queueLengthSum := int64(0)
		maxValueSum := int64(0)
		length := 0
		for _, metrics := range scalersMetrics {
			if metrics.isActive {
				queueLengthSum += metrics.queueLength
				maxValueSum += metrics.maxValue
				isActive = metrics.isActive
				length++
			}
		}
		if length != 0 {
			queueLength = divideWithCeil(queueLengthSum, int64(length))
			maxValue = divideWithCeil(maxValueSum, int64(length))
		}
	case "sum":
		for _, metrics := range scalersMetrics {
			if metrics.isActive {
				queueLength += metrics.queueLength
				maxValue += metrics.maxValue
				isActive = metrics.isActive
			}
		}
	default: // max
		for _, metrics := range scalersMetrics {
			if metrics.queueLength > queueLength && metrics.isActive {
				queueLength = metrics.queueLength
				maxValue = metrics.maxValue
				isActive = metrics.isActive
			}
		}
	}
	maxValue = min(scaledJob.MaxReplicaCount(), maxValue)
	logger.V(1).WithValues("ScaledJob", scaledJob.Name).Info("Checking if ScaleJob scalers are active", "isActive", isActive, "maxValue", maxValue, "MultipleScalersCalculation", scaledJob.Spec.ScalingStrategy.MultipleScalersCalculation)

	return isActive, queueLength, maxValue
}

func (c *ScalersCache) GetMetrics(ctx context.Context, metricName string, metricSelector labels.Selector) ([]external_metrics.ExternalMetricValue, error) {
	var metrics []external_metrics.ExternalMetricValue
	for i, s := range c.scalers {
		m, err := s.scaler.GetMetrics(ctx, metricName, metricSelector)
		if err != nil {
			ns, err := c.refreshScaler(i)
			if err != nil {
				return metrics, err
			}
			m, err = ns.GetMetrics(ctx, metricName, metricSelector)
			if err != nil {
				return metrics, err
			}
		}
		metrics = append(metrics, m...)
	}

	return metrics, nil
}

func (c *ScalersCache) refreshScaler(id int) (scalers.Scaler, error) {
	if id < 0 || id >= len(c.scalers) {
		return nil, fmt.Errorf("scaler with id %d not found. Len = %d", id, len(c.scalers))
	}

	sb := c.scalers[id]
	ns, err := sb.factory()
	if err != nil {
		return nil, err
	}

	c.scalers[id] = scalerBuilder{
		scaler:  ns,
		factory: sb.factory,
	}
	sb.scaler.Close()

	return ns, nil
}

func (c *ScalersCache) GetMetricSpecForScaling() []v2beta2.MetricSpec {
	var spec []v2beta2.MetricSpec
	for _, s := range c.scalers {
		spec = append(spec, s.scaler.GetMetricSpecForScaling()...)
	}
	return spec
}

func (c *ScalersCache) Close() {
	scalers := c.scalers
	c.scalers = nil
	for _, s := range scalers {
		err := s.scaler.Close()
		if err != nil {
			c.logger.Error(err, "error closing scaler", "scaler", s)
		}
	}
}

type scalerMetrics struct {
	queueLength int64
	maxValue    int64
	isActive    bool
}

func (c *ScalersCache) getScaledJobMetrics(ctx context.Context, scaledJob *kedav1alpha1.ScaledJob) []scalerMetrics {
	var scalersMetrics []scalerMetrics
	for i, s := range c.scalers {
		var queueLength int64
		var targetAverageValue int64
		isActive := false
		maxValue := int64(0)
		scalerType := fmt.Sprintf("%T:", s)

		scalerLogger := c.logger.WithValues("ScaledJob", scaledJob.Name, "Scaler", scalerType)

		metricSpecs := s.scaler.GetMetricSpecForScaling()

		// skip scaler that doesn't return any metric specs (usually External scaler with incorrect metadata)
		// or skip cpu/memory resource scaler
		if len(metricSpecs) < 1 || metricSpecs[0].External == nil {
			continue
		}

		isTriggerActive, err := s.scaler.IsActive(ctx)
		if err != nil {
			if ns, err := c.refreshScaler(i); err == nil {
				isTriggerActive, err = ns.IsActive(ctx)
			}
		}

		if err != nil {
			scalerLogger.V(1).Info("Error getting scaler.IsActive, but continue", "Error", err)
			c.recorder.Event(scaledJob, corev1.EventTypeWarning, eventreason.KEDAScalerFailed, err.Error())
			continue
		}

		targetAverageValue = getTargetAverageValue(metricSpecs)

		metrics, err := s.scaler.GetMetrics(ctx, "queueLength", nil)
		if err != nil {
			scalerLogger.V(1).Info("Error getting scaler metrics, but continue", "Error", err)
			c.recorder.Event(scaledJob, corev1.EventTypeWarning, eventreason.KEDAScalerFailed, err.Error())
			continue
		}

		var metricValue int64

		for _, m := range metrics {
			if m.MetricName == "queueLength" {
				metricValue, _ = m.Value.AsInt64()
				queueLength += metricValue
			}
		}
		scalerLogger.V(1).Info("Scaler Metric value", "isTriggerActive", isTriggerActive, "queueLength", queueLength, "targetAverageValue", targetAverageValue)

		if isTriggerActive {
			isActive = true
		}

		if targetAverageValue != 0 {
			maxValue = min(scaledJob.MaxReplicaCount(), divideWithCeil(queueLength, targetAverageValue))
		}
		scalersMetrics = append(scalersMetrics, scalerMetrics{
			queueLength: queueLength,
			maxValue:    maxValue,
			isActive:    isActive,
		})
	}
	return scalersMetrics
}

func getTargetAverageValue(metricSpecs []v2beta2.MetricSpec) int64 {
	var targetAverageValue int64
	var metricValue int64
	var flag bool
	for _, metric := range metricSpecs {
		if metric.External.Target.AverageValue == nil {
			metricValue = 0
		} else {
			metricValue, flag = metric.External.Target.AverageValue.AsInt64()
			if !flag {
				metricValue = 0
			}
		}

		targetAverageValue += metricValue
	}
	count := int64(len(metricSpecs))
	if count != 0 {
		return targetAverageValue / count
	}
	return 0
}

func divideWithCeil(x, y int64) int64 {
	ans := x / y
	remainder := x % y
	if remainder != 0 {
		return ans + 1
	}
	return ans
}

// Min function for int64
func min(x, y int64) int64 {
	if x > y {
		return y
	}
	return x
}