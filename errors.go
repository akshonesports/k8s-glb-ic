package main

import (
	"google.golang.org/api/googleapi"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
)

func isKubernetesError(err error, codes ...v1.StatusReason) bool {
	if err == nil {
		return false
	}

	k8sError, ok := err.(*errors.StatusError)
	if !ok {
		return false
	}

	codeMap := make(map[v1.StatusReason]interface{}, len(codes))
	for _, reason := range codes {
		codeMap[reason] = nil
	}

	_, ok = codeMap[k8sError.ErrStatus.Reason]
	return ok
}

func isComputeError(err error, codes ...string) bool {
	if err == nil {
		return false
	}

	apiError, ok := err.(*googleapi.Error)
	if !ok {
		return false
	}

	codeMap := make(map[string]interface{}, len(codes))
	for _, reason := range codes {
		codeMap[reason] = nil
	}

	for _, err := range apiError.Errors {
		if _, ok := codeMap[err.Reason]; ok {
			continue
		}

		return false
	}

	return true
}

func isComputeNotFound(err error) bool {
	if err == nil {
		return false
	}

	apiError, ok := err.(*googleapi.Error)
	if !ok {
		return false
	}

	return apiError.Code == 404
}
