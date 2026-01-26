package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

type Config struct {
	k *koanf.Koanf
}

func (c *Config) Load() {
	var k = koanf.New(".")

	configPath := FindConfigFilePath()
	if err := k.Load(file.Provider(configPath), yaml.Parser()); err != nil {
		fmt.Println(err)
	}
	k.Load(env.Provider("ENV", ".", func(s string) string {
		return strings.ToLower(
			strings.TrimPrefix(s, "ENV_"),
		)
	}), nil)
	k.Load(env.Provider("AWS", ".", func(s string) string {
		return strings.ToLower(s)
	}), nil)
	c.k = k
}

func (c *Config) Get(key string) interface{} {
	return c.k.Get(key)
}
func (c *Config) String(key string) string {
	return c.k.String(key)
}
func (c *Config) Strings(key string) []string {
	return c.k.Strings(key)
}
func (c *Config) Int(key string) int {
	return c.k.Int(key)
}
func (c *Config) Bool(key string) bool {
	return c.k.Bool(key)
}
func (c *Config) Float64(key string) float64 {
	return c.k.Float64(key)
}

var config *Config

func init() {
	config = &Config{}
	config.Load()
	test()
}

func test() {
	fmt.Println()
}

func FindConfigFilePath() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "config/config.yml"
	}

	possiblePaths := []string{
		filepath.Join(cwd, "config.yml"),
		filepath.Join(cwd, "config.yaml"),
	}

	for _, path := range possiblePaths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "config/config.yml"
}

func GetConfig() *Config {
	return config
}
