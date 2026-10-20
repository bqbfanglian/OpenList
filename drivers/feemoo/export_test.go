package feemoo

import (
	"context"
	"testing"
)

// Test-only bridge for exercising the real fs copier with this driver's mock API.
func NewMockForCopyTests(t *testing.T) (*Feemoo, func(string) []byte) {
	d, m := newMock(t)
	m.maxPart = "3"
	if err := d.authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d, func(name string) []byte {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, task := range m.tasks {
			if task.name == name {
				var data []byte
				for i := 1; i <= len(task.parts); i++ {
					data = append(data, task.parts[i]...)
				}
				return data
			}
		}
		return nil
	}
}
