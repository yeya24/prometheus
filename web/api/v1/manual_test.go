package v1

import (
	"fmt"
	"github.com/prometheus/prometheus/promql"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

func TestLabelNames1(b *testing.T) {
	suite := promql.NewTestWithDir(b, "/home/yeya24/bb")
	defer suite.Close()
	api := &API{
		Queryable:             suite.Storage(),
		QueryEngine:           suite.QueryEngine(),
	}

	//req, err := http.NewRequest(http.MethodGet, `http://example.com/api/v1/labels?match[]={__name__!=""}`, nil)
	//req, err := http.NewRequest(http.MethodGet, `http://example.com/api/v1/labels`, nil)
	req, err := http.NewRequest(http.MethodGet, `http://example.com/api/v1/labels?match[]={account!=""}&match[]={alertname!=""}`, nil)
	require.NoError(b, err)

	//for i := 0; i < 1; i++ {
	res := api.labelNames(req)
	fmt.Println(res.data)
	//}
}

func BenchmarkLabelNames(b *testing.B) {
	suite := promql.NewTestWithDir(b, "/home/yeya24/bb")
	defer suite.Close()
	api := &API{
		Queryable:             suite.Storage(),
		QueryEngine:           suite.QueryEngine(),
	}

	//req, err := http.NewRequest(http.MethodGet, `http://example.com/api/v1/labels?match[]={__name__!=""}`, nil)
	//req, err := http.NewRequest(http.MethodGet, `http://example.com/api/v1/labels`, nil)
	req, err := http.NewRequest(http.MethodGet, `http://example.com/api/v1/labels?match[]={account!=""}&match[]={alertname!=""}`, nil)
	require.NoError(b, err)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := api.labelNames(req)
		fmt.Println(res.data)
	}
}