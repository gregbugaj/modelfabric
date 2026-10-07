package prefixchain

import "testing"

func TestWithSlot(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"the pin goes first and the rest is untouched", `{"model":"m","messages":[]}`, `{"id_slot":3,"model":"m","messages":[]}`},
		{"leading whitespace is kept", " \n{\"model\":\"m\"}", " \n{\"id_slot\":3,\"model\":\"m\"}"},
		{"an empty object gets no trailing comma", `{ }`, `{"id_slot":3 }`},
		{"a body that is not an object is left alone", `[]`, `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(WithSlot([]byte(tc.in), 3)); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
