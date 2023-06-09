// Package testdata implements testing functions used for... testing? our Worker
package testdata

import (
	"math/rand"
	"strings"
	"testing"
)

func TestJustAnAssert(t *testing.T) {
	s := "uh"
	if strings.HasPrefix(s, "u") {
		t.Error("Should not start with 'u'")
	}
}

func TestMultipleRunCalls(t *testing.T) {
	t.Run("This is a test", func(t *testing.T) {
		val := 100
		if rand.Int() == val {
			t.Error("Unlucky")
		}
	})
	t.Run("This is another", func(t *testing.T) {
		val := 200
		if rand.Int() == val {
			t.Error("Unlucky")
		}
	})
}

func TestTable(t *testing.T) {
	tests := []struct{
		name string
		value int
	} {
		{
			name: "hello!",
			value: 5
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != 5 {
				t.Error("value != 5")
			}
		})
	}
}
