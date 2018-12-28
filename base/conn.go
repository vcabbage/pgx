package base

import (
	"crypto/md5"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/pgproto3"
)

// PgError represents an error reported by the PostgreSQL server. See
// http://www.postgresql.org/docs/9.3/static/protocol-error-fields.html for
// detailed field description.
type PgError struct {
	Severity         string
	Code             string
	Message          string
	Detail           string
	Hint             string
	Position         int32
	InternalPosition int32
	InternalQuery    string
	Where            string
	SchemaName       string
	TableName        string
	ColumnName       string
	DataTypeName     string
	ConstraintName   string
	File             string
	Line             int32
	Routine          string
}

func (pe PgError) Error() string {
	return pe.Severity + ": " + pe.Message + " (SQLSTATE " + pe.Code + ")"
}

// DialFunc is a function that can be used to connect to a PostgreSQL server
type DialFunc func(network, addr string) (net.Conn, error)

// ErrTLSRefused occurs when the connection attempt requires TLS and the
// PostgreSQL server refuses to use TLS
var ErrTLSRefused = errors.New("server refused TLS connection")

type ConnConfig struct {
	Host          string // host (e.g. localhost) or path to unix domain socket directory (e.g. /private/tmp)
	Port          uint16 // default: 5432
	Database      string
	User          string // default: OS user name
	Password      string
	TLSConfig     *tls.Config // config for TLS connection -- nil disables TLS
	Dial          DialFunc
	RuntimeParams map[string]string // Run-time parameters to set on connection as session default values (e.g. search_path or application_name)
}

func (cc *ConnConfig) NetworkAddress() (network, address string) {
	// If host is a valid path, then address is unix socket
	if _, err := os.Stat(cc.Host); err == nil {
		network = "unix"
		address = cc.Host
		if !strings.Contains(address, "/.s.PGSQL.") {
			address = filepath.Join(address, ".s.PGSQL.") + strconv.FormatInt(int64(cc.Port), 10)
		}
	} else {
		network = "tcp"
		address = fmt.Sprintf("%s:%d", cc.Host, cc.Port)
	}

	return network, address
}

func (cc *ConnConfig) assignDefaults() error {
	if cc.User == "" {
		user, err := user.Current()
		if err != nil {
			return err
		}
		cc.User = user.Username
	}

	if cc.Port == 0 {
		cc.Port = 5432
	}

	if cc.Dial == nil {
		defaultDialer := &net.Dialer{KeepAlive: 5 * time.Minute}
		cc.Dial = defaultDialer.Dial
	}

	return nil
}

// PgConn is a low-level PostgreSQL connection handle. It is not safe for concurrent usage.
type PgConn struct {
	NetConn           net.Conn          // the underlying TCP or unix domain socket connection
	PID               uint32            // backend pid
	SecretKey         uint32            // key to use to send a cancel query message to the server
	parameterStatuses map[string]string // parameters that have been reported by the server
	TxStatus          byte
	Frontend          *pgproto3.Frontend

	Config ConnConfig
}

func Connect(cc ConnConfig) (*PgConn, error) {
	err := cc.assignDefaults()
	if err != nil {
		return nil, err
	}

	pgConn := new(PgConn)
	pgConn.Config = cc

	pgConn.NetConn, err = cc.Dial(cc.NetworkAddress())
	if err != nil {
		return nil, err
	}

	pgConn.parameterStatuses = make(map[string]string)

	if cc.TLSConfig != nil {
		if err := pgConn.startTLS(cc.TLSConfig); err != nil {
			return nil, err
		}
	}

	pgConn.Frontend, err = pgproto3.NewFrontend(pgConn.NetConn, pgConn.NetConn)
	if err != nil {
		return nil, err
	}

	startupMsg := pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      make(map[string]string),
	}

	// Copy default run-time params
	for k, v := range cc.RuntimeParams {
		startupMsg.Parameters[k] = v
	}

	startupMsg.Parameters["user"] = cc.User
	if cc.Database != "" {
		startupMsg.Parameters["database"] = cc.Database
	}

	if _, err := pgConn.NetConn.Write(startupMsg.Encode(nil)); err != nil {
		return nil, err
	}

	for {
		msg, err := pgConn.ReceiveMessage()
		if err != nil {
			return nil, err
		}

		switch msg := msg.(type) {
		case *pgproto3.BackendKeyData:
			pgConn.PID = msg.ProcessID
			pgConn.SecretKey = msg.SecretKey
		case *pgproto3.Authentication:
			if err = pgConn.rxAuthenticationX(msg); err != nil {
				return nil, err
			}
		case *pgproto3.ReadyForQuery:
			return pgConn, nil
		case *pgproto3.ParameterStatus:
			// handled by ReceiveMessage
		case *pgproto3.ErrorResponse:
			return nil, PgError{
				Severity:         msg.Severity,
				Code:             msg.Code,
				Message:          msg.Message,
				Detail:           msg.Detail,
				Hint:             msg.Hint,
				Position:         msg.Position,
				InternalPosition: msg.InternalPosition,
				InternalQuery:    msg.InternalQuery,
				Where:            msg.Where,
				SchemaName:       msg.SchemaName,
				TableName:        msg.TableName,
				ColumnName:       msg.ColumnName,
				DataTypeName:     msg.DataTypeName,
				ConstraintName:   msg.ConstraintName,
				File:             msg.File,
				Line:             msg.Line,
				Routine:          msg.Routine,
			}
		default:
			return nil, errors.New("unexpected message")
		}
	}
}

func (pgConn *PgConn) startTLS(tlsConfig *tls.Config) (err error) {
	err = binary.Write(pgConn.NetConn, binary.BigEndian, []int32{8, 80877103})
	if err != nil {
		return
	}

	response := make([]byte, 1)
	if _, err = io.ReadFull(pgConn.NetConn, response); err != nil {
		return
	}

	if response[0] != 'S' {
		return ErrTLSRefused
	}

	pgConn.NetConn = tls.Client(pgConn.NetConn, tlsConfig)

	return nil
}

func (c *PgConn) rxAuthenticationX(msg *pgproto3.Authentication) (err error) {
	switch msg.Type {
	case pgproto3.AuthTypeOk:
	case pgproto3.AuthTypeCleartextPassword:
		err = c.txPasswordMessage(c.Config.Password)
	case pgproto3.AuthTypeMD5Password:
		digestedPassword := "md5" + hexMD5(hexMD5(c.Config.Password+c.Config.User)+string(msg.Salt[:]))
		err = c.txPasswordMessage(digestedPassword)
	default:
		err = errors.New("Received unknown authentication message")
	}

	return
}

func (pgConn *PgConn) txPasswordMessage(password string) (err error) {
	msg := &pgproto3.PasswordMessage{Password: password}
	_, err = pgConn.NetConn.Write(msg.Encode(nil))
	return err
}

func hexMD5(s string) string {
	hash := md5.New()
	io.WriteString(hash, s)
	return hex.EncodeToString(hash.Sum(nil))
}

func (pgConn *PgConn) ReceiveMessage() (pgproto3.BackendMessage, error) {
	msg, err := pgConn.Frontend.Receive()
	if err != nil {
		return nil, err
	}

	switch msg := msg.(type) {
	case *pgproto3.ReadyForQuery:
		pgConn.TxStatus = msg.TxStatus
	case *pgproto3.ParameterStatus:
		pgConn.parameterStatuses[msg.Name] = msg.Value
	}

	return msg, nil
}

// ParameterStatus returns the value of a parameter reported by the server (e.g.
// server_version). Returns an empty string for unknown parameters.
func (pgConn *PgConn) ParameterStatus(key string) string {
	return pgConn.parameterStatuses[key]
}
