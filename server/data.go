package server

// Canned data for the demo endpoints. Note that none of it mentions templates:
// a VDP response is ordinary API data plus a view descriptor carried alongside
// it (spec §1).

func navData(active string) []any {
	items := []any{
		map[string]any{"label": "Dashboard", "href": "/dashboard"},
		map[string]any{"label": "Feed", "href": "/feed"},
		map[string]any{"label": "Product", "href": "/product/42"},
		map[string]any{"label": "Sign in", "href": "/login"},
	}
	for _, item := range items {
		m := item.(map[string]any)
		if m["href"] == active {
			m["active"] = true
		}
	}
	return items
}

// dashboardData is the §7.2 dashboard payload: HAL-shaped data, no view info.
func dashboardData(self string) map[string]any {
	return map[string]any{
		"_links": map[string]any{
			"self": map[string]any{"href": self},
		},
		"nav": navData("/dashboard"),
		"stats": map[string]any{
			"revenue": 48200,
			"users":   1847,
			"orders":  312,
		},
		"recentActivity": []any{
			map[string]any{"user": "alice", "action": "purchase", "item": "Widget Pro", "time": "2m ago"},
			map[string]any{"user": "bob", "action": "signup", "time": "15m ago"},
			map[string]any{"user": "carol", "action": "purchase", "item": "Gadget Mini", "time": "41m ago"},
			map[string]any{"user": "dave", "action": "refund", "item": "Widget Pro", "time": "1h ago"},
		},
		"chartData": map[string]any{
			"series": "Daily revenue",
			"labels": []any{"Mon", "Tue", "Wed", "Thu", "Fri"},
			"values": []any{12400, 9800, 14200, 6100, 5700},
		},
	}
}

// feedData backs the slot-array demo (§3.5).
func feedData() map[string]any {
	data := dashboardData("/api/feed")
	data["nav"] = navData("/feed")
	return data
}

// loginData is the §7.1 payload: a form described entirely by data.
func loginData() map[string]any {
	return map[string]any{
		"csrfToken": "abc123",
		"loginUrl":  "/auth/login",
		"fields": []any{
			map[string]any{"name": "username", "type": "text", "label": "Username", "required": true},
			map[string]any{"name": "password", "type": "password", "label": "Password", "required": true},
		},
	}
}

// productData is the §7.4 multi-view payload.
func productData() map[string]any {
	return map[string]any{
		"id":          42,
		"name":        "Widget Pro",
		"price":       29.99,
		"description": "The same JSON renders as a full detail page or a compact card. The server offered both views; the client asked for one.",
		"images":      []any{"front.jpg", "side.jpg", "back.jpg"},
		"reviews": []any{
			map[string]any{"author": "Alice", "rating": 5, "text": "Excellent — exactly what the descriptor promised."},
			map[string]any{"author": "Bob", "rating": 4, "text": "Composes well with my existing slots."},
		},
	}
}

// odataProducts is the §4.3 OData4 payload: a rigid body that cannot carry
// _view, so the descriptor travels by annotation and Link header instead.
func odataProducts(base string) map[string]any {
	return map[string]any{
		"@odata.context": base + "/odata/$metadata#Products",
		"value": []any{
			map[string]any{"ProductID": 1, "Name": "Widget", "Price": 9.99},
			map[string]any{"ProductID": 2, "Name": "Gadget", "Price": 24.99},
		},
	}
}
