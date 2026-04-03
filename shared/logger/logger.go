package logger

import (
	"net"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var Logger *zap.Logger

func InitLogger(logstashAddress string) {
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "timestamp"
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	stdoutCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(os.Stdout),
		zapcore.InfoLevel,
	)

	cores := []zapcore.Core{stdoutCore}
	if logstashAddress != "" {
		conn, err := net.Dial("tcp", logstashAddress)
		if err == nil {
			logstashCore := zapcore.NewCore(
				zapcore.NewJSONEncoder(encoderConfig),
				zapcore.AddSync(conn),
				zapcore.InfoLevel,
			)
			cores = append(cores, logstashCore)
		}
	}

	Logger = zap.New(zapcore.NewTee(cores...), zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))
}
