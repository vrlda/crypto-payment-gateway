package repository

import "strings"

func NormalizeAssetCoin(coin string) string {
	return strings.ToUpper(strings.TrimSpace(coin))
}

func NormalizeAssetNetwork(network string) string {
	switch strings.ToUpper(strings.TrimSpace(network)) {
	case "BTC", "BITCOIN":
		return "BTC"
	case "ETHEREUM", "ERC20":
		return "ERC20"
	case "BSC", "BEP20":
		return "BEP20"
	case "POLYGON", "POL":
		return "POLYGON"
	case "ARBITRUM", "ARB":
		return "ARBITRUM"
	case "TRON", "TRC20":
		return "TRC20"
	case "SOLANA":
		return "SOLANA"
	case "TON":
		return "TON"
	default:
		return strings.ToUpper(strings.TrimSpace(network))
	}
}
