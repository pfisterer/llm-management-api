package main

import app "github.com/pfisterer/llm-management-api/internal"

// @title			LLM Management API
// @version		0.1
// @description	Self-service API of the DHBW LLM service: access, API keys, usage and the Mac fleet.
// @BasePath		/v1
// @securityDefinitions.apikey	BearerAuth
// @in							header
// @name						Authorization
func main() {
	app.RunApplication()
}
