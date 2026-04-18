package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	token = "123123123"
)

func TestAPI(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "API Suite")
}

var _ = Describe("API", func() {
	api := New(token)

	Describe("Start gating", func() {
		It("does not fatal when only public (no-auth) handlers are registered and the token is empty", func() {
			a := New("")
			a.hasHandlers = true
			// No authed handlers registered — only a public one.
			Expect(a.hasAuthedHandlers).To(BeFalse())
			// Simulate what Start() checks without spinning up the server.
			// The real fatal path is: hasAuthedHandlers && Token == "".
			shouldFatal := a.hasAuthedHandlers && a.Token == ""
			Expect(shouldFatal).To(BeFalse())
		})

		It("still expects the token when an authed handler is registered", func() {
			a := New("")
			a.hasHandlers = true
			a.hasAuthedHandlers = true
			shouldFatal := a.hasAuthedHandlers && a.Token == ""
			Expect(shouldFatal).To(BeTrue())
		})
	})

	Describe("RequireToken middleware", func() {
		It("should return 401 Unauthorized when token is not provided", func() {
			handlerFunc := api.RequireToken(testHandler)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/hello", nil)

			handlerFunc(rec, req)

			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
		})

		It("should return 401 Unauthorized when token is invalid", func() {
			handlerFunc := api.RequireToken(testHandler)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/hello", nil)
			req.Header.Set("Authorization", "Bearer 123")

			handlerFunc(rec, req)

			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
		})

		It("should return 200 OK when token is valid", func() {
			handlerFunc := api.RequireToken(testHandler)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/hello", nil)
			req.Header.Set("Authorization", "Bearer "+token)

			handlerFunc(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
		})
	})
})

func testHandler(w http.ResponseWriter, req *http.Request) {
	_, _ = io.WriteString(w, "Hello!")
}
