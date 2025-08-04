// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"k8s.io/apimachinery/pkg/api/resource"
)

// BytesToResource returns the resource.Quantity value for the specified number
// of bytes. If the value is more suitable for BinarySI format, it returns the
// resource.Quantity in BinarySI format, otherwise it returns the resource.Quantity
// in DecimalSI format.
func BytesToResource(b int64) *resource.Quantity {
	maxDivisions := func(b, base int64) int {
		count := 0
		for b > 0 && b%base == 0 {
			b /= base
			count++
		}
		return count
	}

	if maxDivisions(b, 1024) >= maxDivisions(b, 1000) {
		return resource.NewQuantity(b, resource.BinarySI)
	}
	return resource.NewQuantity(b, resource.DecimalSI)
}
