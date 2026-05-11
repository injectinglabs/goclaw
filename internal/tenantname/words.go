package tenantname

// Curated, safe-for-display word lists. All ASCII lowercase, slug-safe.
// Avoid: profanity, brand names, trademarks, geopolitical terms, anything
// that could read as PII or identity claim. Words chosen for soft tone.

var adjectives = []string{
	"amber", "ancient", "balmy", "bold", "brave", "breezy", "bright", "calm",
	"cheery", "clever", "cosmic", "cozy", "crisp", "curious", "dapper", "daring",
	"dewy", "dreamy", "eager", "early", "easy", "elated", "fancy", "fluffy",
	"frosty", "gentle", "glad", "glassy", "happy", "hardy", "hazy", "hopeful",
	"jolly", "kind", "lazy", "lively", "lucky", "lush", "merry", "mild",
	"misty", "neat", "nimble", "noble", "peppy", "plucky", "polished", "quiet",
	"radiant", "rapid", "ready", "rosy", "rustic", "serene", "shiny", "silky",
	"silly", "sleek", "smooth", "snappy", "sparkly", "spry", "stellar", "sunny",
	"swift", "tender", "tidy", "tranquil", "vivid", "warm", "witty", "young",
	"zesty", "soft", "gleaming", "glowing", "humble",
}

var nouns = []string{
	"acorn", "amber", "anchor", "arrow", "aspen", "bay", "beach", "berry",
	"birch", "bloom", "bluff", "breeze", "brook", "canyon", "cedar", "cliff",
	"cloud", "clover", "comet", "coral", "cove", "creek", "crystal", "dawn",
	"delta", "desert", "drift", "dune", "echo", "ember", "fern", "field",
	"flame", "fog", "forest", "fox", "frost", "garden", "geyser", "glade",
	"glow", "grove", "harbor", "heath", "hill", "horizon", "iris", "ivy",
	"jade", "lagoon", "lake", "lily", "lotus", "marsh", "meadow", "mesa",
	"mist", "moon", "moss", "mountain", "oak", "ocean", "opal", "orchid",
	"otter", "pearl", "petal", "pine", "pond", "prairie", "rain", "reef",
	"ridge", "river", "robin", "rose", "ruby", "sage", "sand", "sapphire",
	"shore", "sky", "spark", "spring", "star", "stone", "stream", "summit",
	"sunset", "thicket", "tide", "topaz", "trail", "tundra", "valley", "vine",
	"violet", "willow", "wind", "wood", "wren", "zephyr", "haven", "lantern",
}

var animals = []string{
	"badger", "beaver", "bison", "bunny", "camel", "canary", "caracal", "cardinal",
	"chamois", "cheetah", "chinchilla", "civet", "condor", "coyote", "crane", "deer",
	"dingo", "dolphin", "donkey", "dove", "dragonfly", "duck", "eagle", "egret",
	"elk", "emu", "falcon", "ferret", "finch", "flamingo", "fox", "gazelle",
	"gecko", "giraffe", "goose", "gopher", "grouse", "hare", "hawk", "hedgehog",
	"heron", "hippo", "hummingbird", "ibex", "ibis", "iguana", "impala", "jackal",
	"jaguar", "jay", "kingfisher", "koala", "kookaburra", "lemur", "leopard", "llama",
	"lynx", "macaw", "magpie", "manatee", "marmot", "meerkat", "mink", "mole",
	"mongoose", "moose", "newt", "nightingale", "ocelot", "okapi", "oriole", "osprey",
	"otter", "owl", "panda", "panther", "parrot", "pelican", "penguin", "pheasant",
	"pigeon", "platypus", "puffin", "quail", "quokka", "rabbit", "raccoon", "raven",
	"reindeer", "robin", "salamander", "seal", "serval", "skylark", "sloth", "sparrow",
	"squirrel", "stork", "swallow", "swan", "tapir", "tern", "thrush", "tiger",
	"toucan", "turtle", "vicuna", "wallaby", "walrus", "weasel", "whale", "wolf",
	"wombat", "woodpecker", "yak", "zebra",
}

// Soft syllables (consonant + vowel pairs) for the random-sounding fallback
// when the curated word lists must be avoided. Safe to concatenate two of
// these per group to form readable nonsense like "miro" or "vana".
var softConsonants = []byte{
	'b', 'd', 'f', 'g', 'k', 'l', 'm', 'n', 'p', 'r', 's', 't', 'v', 'z',
}

var softVowels = []byte{'a', 'e', 'i', 'o', 'u'}
