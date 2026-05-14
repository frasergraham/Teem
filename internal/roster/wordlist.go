package roster

// Wordlist is the pool of short, distinctive first-name-ish words the
// allocator draws from when handing out fresh worker ids. Order is
// stable — the allocator walks it linearly and picks the first base
// not yet bound to the requested role, so changing this list mid-
// flight can change which name a freshly-spawned worker gets (but
// never an already-running one). Names are lower-case, ASCII, and
// short enough to stay readable in dashboards. Roughly ~120 entries
// — plenty for any plausible team.
var Wordlist = []string{
	"ada", "blake", "cleo", "drew", "ezra", "fern", "gus", "hana",
	"ivy", "jude", "kai", "lena", "milo", "nora", "otis", "piper",
	"quinn", "river", "sage", "theo", "una", "vega", "wren", "xena",
	"yuki", "zane", "ari", "bex", "cy", "dax", "evi", "finn",
	"gem", "hugo", "indi", "jax", "koa", "liv", "max", "neo",
	"ori", "pax", "quill", "rae", "sol", "taro", "uma", "viv",
	"wes", "yara", "zen", "asa", "bea", "cal", "dee", "eli",
	"fyn", "gia", "hex", "ira", "jem", "kit", "lou", "mae",
	"nyx", "ode", "pip", "qui", "rio", "suki", "tao", "val",
	"wil", "xan", "yoshi", "zora", "bran", "dorian", "esme", "faye",
	"gail", "hux", "isla", "jules", "knox", "leif", "marlowe", "niko",
	"oren", "pearl", "quincy", "remi", "shae", "tate", "ula", "vale",
	"wynn", "xander", "yael", "zara", "amos", "boe", "ciel", "dune",
	"eve", "flynn", "gale", "harlow", "iggy", "june", "kaz", "lex",
	"mira", "nash", "ollie", "perry", "ren", "sloane", "thad", "ume",
	"vince", "wade",
}
