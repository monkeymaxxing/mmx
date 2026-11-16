package modules

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// postgresqlPasswordAsWritten marks a password that SASLprep leaves as it is or refuses: the
// server hashes its bytes as they were written.
const postgresqlPasswordAsWritten = "\x00as written"

// The eight passwords that the measuring set on PostgreSQL 13.23 (Rocky
// Linux 9), each with the form the server stored. They cover the three
// branches of postgresqlPreparedPassword: ASCII bytes as they are, a text
// that SASLprep changes, and a text that SASLprep refuses.
var postgresqlPasswordsMeasuredFirst = []struct{ password, prepared, stored string }{
	{"correct horse 1", postgresqlPasswordAsWritten, "SCRAM-SHA-256$4096:jIROvarGVfF0pvd27jw2ww==$BHcH1ilVFXZPqotkzSAs7sWnmxdIHv13TtWIIsQ3DhI=:oF4s12tfMAHBjU6k+D33Zv5032TRwLoN1fyRZczuH9s="},
	{`pa'ss\wo"rd`, postgresqlPasswordAsWritten, "SCRAM-SHA-256$4096:jTaNX1teKJdv5zFRriSt3g==$SQr8UYQ7jQZChQUX1clrcmq16d90T6nR4NjpbiQCLwY=:vZrwJCKybkXbmhlICK3CCemfHFkuEOEfDkDnPIMVsDU="},
	{`pa:ss\wo:rd`, postgresqlPasswordAsWritten, "SCRAM-SHA-256$4096:0oIpDdvnMEwo7kGvC02g/A==$GuNl2z8uslixfs3UEsogyOJGFhwBu2LCnPIGR8Jqdtc=:u150RP8Y2Ab26bsUZvL5ng3AQEI6ZhtMsjF4cTpPfFQ="},
	{"pässwörd-ñ", postgresqlPasswordAsWritten, "SCRAM-SHA-256$4096:U4hRNGslBP9LOEQ+kvKScg==$ReGkJMXlfPDoIvrG07Jl+4B4JqNgAkEaNUuTkZzbbvg=:H7IfGsBwFh9BueyeFoVaxzB/2mgbCXGvVfhbPrLL8yY="},
	{"ªbⅨ\u00adc", "abIXc", "SCRAM-SHA-256$4096:8dHOb+79RD+QZbXtIBGuVg==$9yNzGfKQeHq5zNect+VuRI2DlpvDfPlqSChGpIQMdvs=:hhe+qkIF+q8LVM8GWOa9GFuTTacqIsQqqLTGJT32/qw="},
	{"päss", "päss", "SCRAM-SHA-256$4096:YF+O6SmzGLESIdbZaXArxA==$GR9eT7ycAQf75oRT18Jv9BOECCfUB8WH62XJA00ZA9I=:uC6PFBH/4gfab46a59fYSNMjalgHKr1MeFq4zbQpopU="},
	{"ª\u0007x", postgresqlPasswordAsWritten, "SCRAM-SHA-256$4096:QZFrJ1PlrLCR7v5qWQgPVQ==$PjFUABGExrk+TRHC+/eN18aMpuZGuF4aq8NMjxOSOy0=:Ebm/oSLi/bXpqBpf3Vt3wNmhILqQxzEle/N63+1oAw0="},
	{"ª\U0001f600", postgresqlPasswordAsWritten, "SCRAM-SHA-256$4096:D+F7Dw4LgQ42RS2JPNa0KQ==$f2/3Njokp/gZEp0kV0wQrllphXPk93r4JrfiKhFk/MY=:VxrkYe6JOno/5P5g8jqncTw+VVvVt67cXE6bVnFenzI="},
}

// Passwords at the edges of SASLprep, each as the hexadecimal digits of its
// UTF-8 bytes, with the form that a real server made of it: PostgreSQL 13.23
// on Rocky Linux 9 and PostgreSQL 17.11 on Debian 13 (ALTER ROLE with the
// clear password, password_encryption scram-sha-256). They pin down what
// the RFC leaves open and PostgreSQL decides: an ASCII password keeps its
// control characters, a zero width space becomes a space, and the checks
// read the text from before the normalization, so a code point that Unicode
// 3.2 lacks is refused even where NFKC would turn it into ASCII.
var postgresqlPasswordsAtTheEdges = []struct{ what, hexadecimal, prepared, storedBy13, storedBy17 string }{
	{"ascii with a control character", "610762", postgresqlPasswordAsWritten,
		"SCRAM-SHA-256$4096:jlgWZlJtV7124TG3nh39oA==$on/MC6kGf/KWSGLehZkWVAalKPMC4lyMREmKPWKjizY=:cwWnWnYr+KERC3C9ACWnslX5NjXUHu7W8jWsph7MX9k=",
		"SCRAM-SHA-256$4096:rYQGV/rfgrpAo8M1EGW/4A==$f3obSfkl9HrYW62efH4nCW7uKtQUzPx94dJwDSfOvPk=:VAlr0H9qPzX5hv1G2kxHXFcBDyQGmtDNhfnK1nUud5I="},
	{"ascii with a tab", "7461620968657265", postgresqlPasswordAsWritten,
		"SCRAM-SHA-256$4096:RTzIYdSma1piWk8XxXeKzA==$/tj8RYcXuPUXCZ8smWPLW/sElJbQNqGGDqx+Xp8OI9M=:E5rMv2UXoo1b3p3YZbRKIcCGdok+p9iYpPwh0q0D9I0=",
		"SCRAM-SHA-256$4096:0nvj313xCOINAMnT+W/56A==$yhpi9QCBb0S7NS0w0zxLMmV5FABZrVOt07Xc5h/0mA4=:iNzUm3u6g4Ynf0fg/RtgFW2kub6lCpEpwdF5OIIaqmE="},
	{"zero width space between letters", "61e2808b62", "a b",
		"SCRAM-SHA-256$4096:7G8oaIgsKf2L9F4otHTnDQ==$Juwg6mdylHWmfopW7VYv3pbiLbAaHYpKY34zRHERg0s=:vLKiqFJTkMdA1RiquprO39ZS+Xt/gXktVXOWGJrccbg=",
		"SCRAM-SHA-256$4096:nOV/qu7d1mN89n/QukdlVw==$gbQS3g2sMa19OptUxn+9yhfbOxwLTv4uCJDrMGhtC08=:+a3z7zkI9MnC0nYi1tN3BYd1bsMx4W4lNUwkeOKRInw="},
	{"code point newer than unicode 3.2 that nfkc changes", "61f09f8480", postgresqlPasswordAsWritten,
		"SCRAM-SHA-256$4096:k6yd3XuTaB1X7jjInc7dbw==$Wvi1FbjsxDjoFLUgVOUmGHa1/5RAzjsA9ASWrBseE9w=:Lo9yuOZ4JVH7l9a397iD4FKmMv7D9ilWSOdLMGv3vOg=",
		"SCRAM-SHA-256$4096:S0/YqG/Dskx/8o6Gj7zNxg==$aZnNOF8Sx7geCa03a7sZI9CuEF11KI/Vg3i+SFhydBw=:XKGkuB2C5td0tpRFKVb8QHWJU0JDSTfiYB3nb5Z08yU="},
	{"only a soft hyphen", "c2ad", postgresqlPasswordAsWritten,
		"SCRAM-SHA-256$4096:FeiBLV/79BQB56e1ChGYhg==$JodWAq4LWuOV/rnjxz2ofYFIzaRMsLUivFO4AQniHdo=:LNGUL5oI7iemN+6D7E7juxrB0wbc5G2m2/1oiRaS1xY=",
		"SCRAM-SHA-256$4096:qrQjsa0hriFlcm/oiKeagA==$4ZzhaKNsg4BsN5bHUgYk7PH0RZ98RB+lY+/RjIk2gJg=:9DWB3iRsPz/R73xJwaRJbjX+CyKGzmOFVmDZoRxF55o="},
	{"right to left with a left to right letter", "d79061d790", postgresqlPasswordAsWritten,
		"SCRAM-SHA-256$4096:hP98wwSnZepeU290njZZRQ==$QaV6tcGahoOpBSg+6jlM4cVl93a/GGuVCaQ8Kfeda4o=:d/M6n+Wje7tNF11cn1zIs7NAVOUhYKw5KCPULQpyXx8=",
		"SCRAM-SHA-256$4096:DI/HJxcbipugC6oduOPtCA==$9V/VSYo2azw6kYGjHPt2WvHm/xrJpAW7Qcj9SGAa6gY=:dD1JnjnmXxcmZdgE5g5GkVeAOzV89jthdhvEfmourr4="},
	{"right to left with a digit", "d79031d790", "\u05d01\u05d0",
		"SCRAM-SHA-256$4096:R/tj1w2kA5zhbzvv7JCa9w==$y8iGHJHuoqgfzTW3RjI5vJMTbsZ+mAx3JLzGQpkgG0k=:Q7zSvjzcP46B4qy3P/sMs7qfauIvqRdaCOWnY0bmOOs=",
		"SCRAM-SHA-256$4096:zcwqQAb5wVv7+TT13z4oNQ==$fTBj0jp+cRtAwfj8/IsvYL2TGk+TVH5YvWn6PHuAEP4=:wD9/EZJKffYqAb+yKSBJ1tjhGMmPpoQj7tAoFdLDPfc="},
	{"right to left first but not last", "d79031", postgresqlPasswordAsWritten,
		"SCRAM-SHA-256$4096:1dwnMcogku1Czj7svPLsDA==$AuyZMaYcTW6caATTbzZeFFmAaJQnZNoSuskg+3PVqNc=:cd8HFC8WhK4Eq0D7U2/N5lbv6PwIRXarqnRMIIcZ5U8=",
		"SCRAM-SHA-256$4096:oBJr+tGABHN4XUa31v1fow==$DQ2C+jzzh3dUQ18CmRjkjnGR7Quu7Kvto5OX7tGdsc0=:NbOAWm+bQ+JktOCimIYsmYFP/xYWNoOtPYk2rOyh2jE="},
	{"prohibited before normalization only", "61cd80", postgresqlPasswordAsWritten,
		"SCRAM-SHA-256$4096:ObFPGGvVAXzxoAfYkAIwWw==$18WaGvx6+3SZsSTe/EmQd7gl0x8rcdGVZohKAD6uhd4=:0P2gmicDhxwTamNNaeFxhCMJ5+RTOw7gxsf7k+Wzrlk=",
		"SCRAM-SHA-256$4096:tFtT8TwP0ZqxPKGdQNKAWA==$uDNT+d3vfeDHqeU2SwKbH4EjhRPLY+nwzIbAdvbIQkc=:kJtfcgw0Lpb4LI3R58cEoAL7lrl6AOhWy3WuEO07AGI="},
	{"roman numeral nine", "e285a8", "IX",
		"SCRAM-SHA-256$4096:YcRfTtKp/GL2uOllPE+QkA==$ko7sSV2PtPj3wxeN5nKZ4T1uROY4BJnAmdffahE1wHs=:vdTu/wYS/Mlu56uHCB/wuFV0gAdhj5Ucfq797c+LfzQ=",
		"SCRAM-SHA-256$4096:aVRb9jn86X/2DHR0cbTDSw==$QxOtatuNjhoJ+SDgZwMR+e49BPSkjEWs9VDl2PGI/8c=:qiE/rzAJ3kDaZJsbf8qXJpRJtFyHeQWjlGoBcgOFQIw="},
	{"fullwidth letters", "efbd81efbd82efbd83", "abc",
		"SCRAM-SHA-256$4096:iYaBVZ13qvjZYDljOnhQMA==$Gq6dD72knyato/NCF+aeg4yRoFSp9BfZ7xwSXrMvNdA=:38MhqDr+4siIbQQr1C83sTKNXbJRRvVZ/H6bSiwkuc8=",
		"SCRAM-SHA-256$4096:F+BoIy+xlRBBkLVIQYDEIw==$Li8kLvnNZsqZ11KAymPrDvqa+VtWjjwQ59OQ2pM5kig=:2/rZK7QRFllwr0UXT8psYFxArCnCvN+8MjyTrn9sRdw="},
	{"no break space between letters", "61c2a062", "a b",
		"SCRAM-SHA-256$4096:iWGIAZ5j7ipMAZvjRXSAGA==$qL1AhU9S9UCWfoq9Pn2XhWcJSpg4xpyFlEwfoa0faAw=:AGtwWSqvlSl6w1fP+rEEMeEyV/GKaAmb/DvIwkGSkrU=",
		"SCRAM-SHA-256$4096:uUFjRjUTZftxd+p3j1X8EQ==$GupnWHcjW5RDFyBa4j0UpGO1StYjUGsjfG3N0YQvYCg=:WkP3IDHdlVWn4pAQWvZLQaDLzn0cFNiNG7QfS53Q/rM="},
	{"letter and combining ring", "41cc8a", "\u00c5",
		"SCRAM-SHA-256$4096:o/3i1vvlwmruibuXo/QPXA==$WL2UNbLuClpo7lJXziDjp14QW1ASjyjdISnHp+yIQ6w=:Fe4ihvRDcTEZGcXzxIX/sIajXyWhawrzXuge9hAaBr4=",
		"SCRAM-SHA-256$4096:+1wNhEo44kzhN+hzJzrLRg==$XcwwQqsfpbKVS6LF0n39HGFsbAWYhS2sTpkjRjGKp/Y=:iVq8l/A0WToVBgyqKFHX91sHOz2lCop+/jliFv4PWp0="},
	{"fi ligature", "efac81", "fi",
		"SCRAM-SHA-256$4096:dxOPG/jq97GswEA1aw5SyA==$4c8pi4va82rb3tvSnK5YFF0wgt9KyDT50thXhRLSzMY=:t1SOTmmrPafApl+q/FqP9eWywJNru0ifgouJ1bZ2wA8=",
		"SCRAM-SHA-256$4096:CWLV/7VlgXMEaosu9UmiQQ==$BQhGG0Cwofz1Hf7tkmqjdlqTYFRAJM3dvGhGWtzWALw=:X5FFYM0ftxaaOJfq8tBSxaQg7Qh04iX6MSdBiY/tbps="},
	{"soft hyphen between letters", "49c2ad58", "IX",
		"SCRAM-SHA-256$4096:eBz45avP+mOgzmlDBiiVwg==$L4zqLKeVzh1UQs0NGLxF4ETrLLMsS1Eu6Bzjr/1GzWI=:NM7glfm/BpTrNg17kZ2R2Y4Iyx6/VCebPHa3h7TVvC0=",
		"SCRAM-SHA-256$4096:lhRbak5l9DqTtSBodLvvSg==$7waqyOXhD5enKfH+lIj/dacBrBtvwMuG/n6M15BUPJ0=:T/JJUxr9vXfDiiogPwoRdE15zuIWL9qjjkoOg4MViM0="},
	{"unassigned in unicode 3.2", "c8a1", postgresqlPasswordAsWritten,
		"SCRAM-SHA-256$4096:rPiODpM3oSVzRNpdViqg9w==$cXv+gfUm7IcE0VO3D5p00nH77dwmW+xnCSdPh3JuUk4=:ZXBuTnqcHUzaLMeSQboyQ0PTvt9yiyauZ3BquuFYgus=",
		"SCRAM-SHA-256$4096:yk6yZBs2veQtPIkloIEBCw==$5R0Kebo888mCA9bGZjs3d150kNhMdTMKiPGwcRP0h5k=:NvwOhRs3CrNi2XxJNY5pVZtfJGJtVUKX2r5i840sZsU="},
	{"compatibility ideograph of corrigendum 3", "efa591", "\u964b",
		"SCRAM-SHA-256$4096:mRdc/wkI7OhLc9ZTEpz2cg==$6mFudoR5xVIMn74WN8wulH2/5JbQJwo7iQ4tZy1Zv7s=:SfiEt3OW+B76fvo1+ucJ5ST3/WtKJwuUyK8GxeTyTTQ=",
		"SCRAM-SHA-256$4096:LsJr7PGMiW0jUtnouUYz9g==$agyhNqh3hG9oJ5Z+F4Zr4zYWCknlAV2aw/W5HN4Uyc4=:HYpR+AqPBSf4ny3A9tdMpjy8qZWoAV1Y/gSwqafDeOM="},
	{"compatibility ideograph of corrigendum 4", "f0afa1a8", "\u36fc",
		"SCRAM-SHA-256$4096:FnJeNAIjAVrprLZJELhgCQ==$NCYUSZMZ6vv5VHePiZ2RaBfDabeIIHkGu6CasB8pwI0=:FnAaJzo2BquQU2m2XkUTlm+2aSmtS9R+CXC/cmrT13g=",
		"SCRAM-SHA-256$4096:bOL+PtgeY+obRcfMROFepA==$cNCcs37wBpM+HzZgqquV21l3qX0eEXYtEIykq7cLcYU=:FtivhNPMtRIi4mrL53QMOmwhWWBgbnt00FEolgp6J/U="},
	{"hangul jamo that compose", "e18480e185a1", "\uac00",
		"SCRAM-SHA-256$4096:4U5csELqfOfbrcOPbolekg==$VRgfmqbFV9xwfKejR2h40OnQmf8SVlJRKdOQT4emLi0=:isbOR/6zHG32i4jo9DLR0TxQ51KV7oqSncEz4+GEy54=",
		"SCRAM-SHA-256$4096:JP+ERJC/u3/OtrSmutoQcg==$f0ArJ4AdkXSrDJGGVIpJJRCd/kDuviCG/LtjkLc+ZvA=:RjTpTcivIATJT9Yr44rqM9VllHkbkHqgBkAuGtJxLQU="},
	{"greek with tonos decomposed", "ceb1cc81", "\u03ac",
		"SCRAM-SHA-256$4096:EdgkvOt686Hnl1Hi4lsisw==$e00PxorrSSGb9xLGWvJkFaBxaKhOQiwn8lVPgCLM7PI=:b9pdofBwUoHmWnG2DN58u8UF6oElr9Rphj+KX4CSuCc=",
		"SCRAM-SHA-256$4096:fRJIp+BDiyHSzf0lHl4/Ew==$SIIYIPsEWmMHkt/klo1ryTGcOJ7V99oJgWNkowLljEw=:wJDlAp2E5+7ZjNWnzVupVtIviYcfpL/PSRoB5Fk0cNY="},
	{"ohm sign", "e284a6", "\u03a9",
		"SCRAM-SHA-256$4096:eYu5y7Ysjild38oI5ZhCxg==$lodlM2l6iTtm24OuJDCZ7IaoqYOqO0wU3LKBbZH/seo=:Rb4T+tRICLQIjmhGqk0I0Q1fuVMjfc0/e0a9hCtFU8o=",
		"SCRAM-SHA-256$4096:vil3gkpCSDM5iroyAnTuKA==$yq1+lBAfiygw9kvNuhxx8pv9Bdj7VNCSuVpGDoxR3wo=:KyMNXt14qhqr5PXtvoRA/z0QdY/S6bgo2MB+MAZiRgg="},
	{"three hundred letters", "4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c", postgresqlPasswordAsWritten,
		"SCRAM-SHA-256$4096:jEzFLBEMAktsuWxhUp7/iA==$bkrAhyFhJbGatmif1qZPGXVQNPZuLt4A6GQn4Pu9RbQ=:nnuP4OUKQ0wOc5gRYeZGN8dmA2hDGZUJfs2OggAHCAI=",
		"SCRAM-SHA-256$4096:3iUGpxdvsbhSYddsPiIUDA==$pDdo0vUBrnJ7EVXuVQ3UwNbl77jGgLLAcrKQ8JMN1U0=:5gQPX+6LmCrCMMBznxBHlvGKH6p7vn70MBkX5L3E6D4="},
}

func TestPostgresqlPreparedPasswordIsTheOneTheServerHashes(t *testing.T) {
	t.Parallel()
	check := func(what, password, prepared string, storedForms ...string) {
		t.Helper()
		if prepared == postgresqlPasswordAsWritten {
			prepared = password
		}
		if got := string(postgresqlPreparedPassword(password)); got != prepared {
			t.Errorf("%s: prepared %q, want %q", what, got, prepared)
		}
		for _, stored := range storedForms {
			if !postgresqlStoredFormHolds(stored, password) {
				t.Errorf("%s: the form that the server stored is not the form MMX computes", what)
			}
			if postgresqlStoredFormHolds(stored, password+"x") {
				t.Errorf("%s: another password matched the stored form", what)
			}
		}
	}
	for _, measured := range postgresqlPasswordsMeasuredFirst {
		check(measured.password, measured.password, measured.prepared, measured.stored)
	}
	for _, edge := range postgresqlPasswordsAtTheEdges {
		password, err := hex.DecodeString(edge.hexadecimal)
		if err != nil {
			t.Fatal(edge.what, err)
		}
		check(edge.what, string(password), edge.prepared, edge.storedBy13, edge.storedBy17)
	}
	// Bytes that are not UTF-8 are hashed as they are.
	if got := string(postgresqlPreparedPassword("p\xe4ss")); got != "p\xe4ss" {
		t.Errorf("a password that is not UTF-8 was prepared as %q", got)
	}
}

// A password that SASLprep changes is stored by its prepared text: the form
// of the bytes as written is another one, which no client matches at login.
func TestPostgresqlScramFormOfAPreparedPassword(t *testing.T) {
	t.Parallel()
	salt := []byte("0123456789abcdef")
	changed, err := postgresqlScramForm("ªbⅨ\u00adc", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if prepared, _ := postgresqlScramForm("abIXc", salt, 4096); changed != prepared {
		t.Error("the form of a password and of its prepared text differ")
	}
	if !postgresqlStoredFormHolds(changed, "abIXc") {
		t.Error("the prepared text does not match the form")
	}
}

func TestNewPostgresqlScramForm(t *testing.T) {
	t.Parallel()
	first, err := newPostgresqlScramForm("correct horse 1", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newPostgresqlScramForm("correct horse 1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two forms have the same salt")
	}
	// As the server makes it: 4096 iterations, a salt of 16 bytes, 133
	// characters in all.
	salting, _, _ := strings.Cut(strings.TrimPrefix(first, postgresqlScramPrefix), "$")
	iterations, saltText, _ := strings.Cut(salting, ":")
	salt, err := base64.StdEncoding.DecodeString(saltText)
	if err != nil || len(salt) != 16 || iterations != "4096" || len(first) != 133 {
		t.Fatalf("%s: %d characters, %s iterations, a salt of %d bytes, %v", first, len(first), iterations, len(salt), err)
	}
	if !postgresqlStoredFormHolds(first, "correct horse 1") || postgresqlStoredFormHolds(first, "correct horse 2") {
		t.Error("the new form does not hold exactly its password")
	}
	// The server's own iteration count, where it has the setting.
	counted, err := newPostgresqlScramForm("correct horse 1", 10000)
	if err != nil || !strings.HasPrefix(counted, "SCRAM-SHA-256$10000:") || !postgresqlStoredFormHolds(counted, "correct horse 1") {
		t.Error(counted, err)
	}
}

// What is no SCRAM form of the password: nothing, an md5 form (also of the
// same password: MMX replaces it), and a text that only looks like a form.
func TestPostgresqlStoredFormsThatDoNotHoldThePassword(t *testing.T) {
	t.Parallel()
	// MD5("correct horse 1" + "pw_md5hba"), as PostgreSQL 13 stored it.
	md5Form := "md5d80d949e1be2dd4a1cfa9e41f5240240"
	if !postgresqlStoredFormIsMD5(md5Form) || postgresqlStoredFormHolds(md5Form, "correct horse 1") {
		t.Error("an md5 form was not recognized, or counts as the password's SCRAM form")
	}
	for _, stored := range []string{"", "md5nothex", "correct horse 1", "SCRAM-SHA-256$4096:not-base64!$abc:def", "SCRAM-SHA-256$0:AAAA$abc:def", "SCRAM-SHA-256$4096", "SCRAM-SHA-256$4096:AAAAAAAAAAAAAAAAAAAAAA==$short:short"} {
		if postgresqlStoredFormHolds(stored, "correct horse 1") || postgresqlStoredFormIsMD5(stored) {
			t.Errorf("%q counts as a stored form of the password", stored)
		}
	}
}

func TestSASLprepTables(t *testing.T) {
	t.Parallel()
	for name, ranges := range map[string]codePointRanges{"spaces": saslprepNonASCIISpaces, "nothing": saslprepMappedToNothing, "prohibited": saslprepProhibited,
		"unassigned": saslprepUnassigned, "right to left": saslprepRightToLeft, "left to right": saslprepLeftToRight} {
		for i, r := range ranges {
			if r.low > r.high || (i > 0 && r.low <= ranges[i-1].high+1) {
				t.Errorf("%s: range %d (%U to %U) is not sorted apart from the one before", name, i, r.low, r.high)
			}
		}
	}
	// The number of ranges in each table of RFC 3454, as PostgreSQL carries them.
	if len(saslprepNonASCIISpaces) != 6 || len(saslprepMappedToNothing) != 8 || len(saslprepProhibited) != 34 ||
		len(saslprepUnassigned) != 396 || len(saslprepRightToLeft) != 34 || len(saslprepLeftToRight) != 360 {
		t.Error("a table of RFC 3454 has another number of ranges")
	}
	for r, want := range map[rune]bool{0x00A0: true, 0x2000: true, 0x200B: true, 0x200C: false, 0x0020: false, 0x3000: true, 0x3001: false} {
		if saslprepNonASCIISpaces.has(r) != want {
			t.Errorf("%U in the table of spaces: %v", r, !want)
		}
	}
	if !saslprepUnassigned.has(0x0221) || saslprepUnassigned.has(0x0220) || !saslprepUnassigned.has(0x1F600) || !saslprepRightToLeft.has(0x05D0) || !saslprepLeftToRight.has('a') || saslprepLeftToRight.has('1') {
		t.Error("a code point is in the wrong table")
	}
}
